package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
	"github.com/docker/distribution/digest"
	distreference "github.com/docker/distribution/reference"
	"github.com/docker/docker/api"
	"github.com/docker/docker/cliconfig"
	"github.com/docker/docker/image"
	versionPkg "github.com/docker/docker/pkg/version"
	"github.com/docker/docker/reference"
	"github.com/docker/docker/registry"
	types "github.com/docker/engine-api/types"
	containerTypes "github.com/docker/engine-api/types/container"
	registryTypes "github.com/docker/engine-api/types/registry"
	"golang.org/x/net/context"
)

// fallbackError wraps an error that can possibly allow fallback to a different
// endpoint.
type fallbackError struct {
	// err is the error being wrapped.
	err error
	// confirmedV2 is set to true if it was confirmed that the registry
	// supports the v2 protocol. This is used to limit fallbacks to the v1
	// protocol.
	confirmedV2 bool
}

// Error renders the FallbackError as a string.
func (f fallbackError) Error() string {
	return f.err.Error()
}

type manifestFetcher interface {
	Fetch(ctx context.Context, ref reference.Named) (*imageInspect, error)
}

type imageInspect struct {
	Tag             string
	Digest          string
	RepoTags        []string
	Comment         string
	Created         string
	ContainerConfig *containerTypes.Config
	DockerVersion   string
	Author          string
	Config          *containerTypes.Config
	Architecture    string
	Os              string
}

func validateName(name string) error {
	distref, err := distreference.ParseNamed(name)
	if err != nil {
		return err
	}
	hostname, _ := distreference.SplitHostname(distref)
	if hostname == "" {
		return fmt.Errorf("Please use a fully qualified repository name")
	}
	return nil
}

func inspect(c *cli.Context) (*imageInspect, error) {
	name := c.Args().First()
	if err := validateName(name); err != nil {
		return nil, err
	}
	ref, err := reference.ParseNamed(name)
	if err != nil {
		return nil, err
	}
	imgInspect, err := getData(c, ref)
	if err != nil {
		return nil, err
	}
	return imgInspect, nil
}

func getData(c *cli.Context, ref reference.Named) (*imageInspect, error) {
	repoInfo, err := registry.ParseRepositoryInfo(ref)
	if err != nil {
		return nil, err
	}
	authConfig, err := getAuthConfig(c, repoInfo.Index)
	if err != nil {
		return nil, err
	}
	if err := validateRepoName(repoInfo.Name()); err != nil {
		return nil, err
	}
	//options := &registry.Options{}
	//options.Mirrors = opts.NewListOpts(nil)
	//options.InsecureRegistries = opts.NewListOpts(nil)
	//options.InsecureRegistries.Set("0.0.0.0/0")
	//registryService := registry.NewService(options)
	registryService := registry.NewService(nil)
	//// TODO(runcom): hacky, provide a way of passing tls cert (flag?) to be used to lookup
	//for _, ic := range registryService.Config.IndexConfigs {
	//ic.Secure = false
	//}

	endpoints, err := registryService.LookupPullEndpoints(repoInfo)
	if err != nil {
		return nil, err
	}

	var (
		ctx                    = context.Background()
		lastErr                error
		discardNoSupportErrors bool
		imgInspect             *imageInspect
		confirmedV2            bool
	)

	for _, endpoint := range endpoints {
		// make sure I can reach the registry, same as docker pull does
		v1endpoint, err := endpoint.ToV1Endpoint(nil)
		if err != nil {
			return nil, err
		}
		if _, err := v1endpoint.Ping(); err != nil {
			if strings.Contains(err.Error(), "timeout") {
				return nil, err
			}
			continue
		}

		if confirmedV2 && endpoint.Version == registry.APIVersion1 {
			logrus.Debugf("Skipping v1 endpoint %s because v2 registry was detected", endpoint.URL)
			continue
		}
		logrus.Debugf("Trying to fetch image manifest of %s repository from %s %s", repoInfo.Name(), endpoint.URL, endpoint.Version)

		//fetcher, err := newManifestFetcher(endpoint, repoInfo, config)
		fetcher, err := newManifestFetcher(endpoint, repoInfo, authConfig, registryService)
		if err != nil {
			lastErr = err
			continue
		}

		if imgInspect, err = fetcher.Fetch(ctx, ref); err != nil {
			// Was this fetch cancelled? If so, don't try to fall back.
			fallback := false
			select {
			case <-ctx.Done():
			default:
				if fallbackErr, ok := err.(fallbackError); ok {
					fallback = true
					confirmedV2 = confirmedV2 || fallbackErr.confirmedV2
					err = fallbackErr.err
				}
			}
			if fallback {
				if _, ok := err.(registry.ErrNoSupport); !ok {
					// Because we found an error that's not ErrNoSupport, discard all subsequent ErrNoSupport errors.
					discardNoSupportErrors = true
					// save the current error
					lastErr = err
				} else if !discardNoSupportErrors {
					// Save the ErrNoSupport error, because it's either the first error or all encountered errors
					// were also ErrNoSupport errors.
					lastErr = err
				}
				continue
			}
			logrus.Debugf("Not continuing with error: %v", err)
			return nil, err
		}

		return imgInspect, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no endpoints found for %s", ref.String())
	}

	return nil, lastErr
}

func newManifestFetcher(endpoint registry.APIEndpoint, repoInfo *registry.RepositoryInfo, authConfig types.AuthConfig, registryService *registry.Service) (manifestFetcher, error) {
	switch endpoint.Version {
	case registry.APIVersion2:
		return &v2ManifestFetcher{
			endpoint:   endpoint,
			authConfig: authConfig,
			service:    registryService,
			repoInfo:   repoInfo,
		}, nil
	case registry.APIVersion1:
		return &v1ManifestFetcher{
			endpoint:   endpoint,
			authConfig: authConfig,
			service:    registryService,
			repoInfo:   repoInfo,
		}, nil
	}
	return nil, fmt.Errorf("unknown version %d for registry %s", endpoint.Version, endpoint.URL)
}

func getAuthConfig(c *cli.Context, index *registryTypes.IndexInfo) (types.AuthConfig, error) {
	var (
		username = c.GlobalString("username")
		password = c.GlobalString("password")
		cfg      = c.GlobalString("docker-cfg")
	)
	if _, err := os.Stat(cfg); err != nil {
		logrus.Infof("Docker cli config file %q not found: %v, falling back to --username and --password if needed", cfg, err)
		return types.AuthConfig{
			Username: username,
			Password: password,
			Email:    "stub@example.com",
		}, nil
	}
	confFile, err := cliconfig.Load(cfg)
	if err != nil {
		return types.AuthConfig{}, err
	}
	authConfig := registry.ResolveAuthConfig(confFile.AuthConfigs, index)
	logrus.Debugf("authConfig for %s: %v", index.Name, authConfig)

	return authConfig, nil
}

func validateRepoName(name string) error {
	if name == "" {
		return fmt.Errorf("Repository name can't be empty")
	}
	if name == api.NoBaseImageSpecifier {
		return fmt.Errorf("'%s' is a reserved name", api.NoBaseImageSpecifier)
	}
	return nil
}

func makeImageInspect(img *image.Image, tag string, dgst digest.Digest, tagList []string) *imageInspect {
	var digest string
	if err := dgst.Validate(); err == nil {
		digest = dgst.String()
	}
	return &imageInspect{
		Tag:             tag,
		Digest:          digest,
		RepoTags:        tagList,
		Comment:         img.Comment,
		Created:         img.Created.Format(time.RFC3339Nano),
		ContainerConfig: &img.ContainerConfig,
		DockerVersion:   img.DockerVersion,
		Author:          img.Author,
		Config:          img.Config,
		Architecture:    img.Architecture,
		Os:              img.OS,
	}
}

func makeRawConfigFromV1Config(imageJSON []byte, rootfs *image.RootFS, history []image.History) (map[string]*json.RawMessage, error) {
	var dver struct {
		DockerVersion string `json:"docker_version"`
	}

	if err := json.Unmarshal(imageJSON, &dver); err != nil {
		return nil, err
	}

	useFallback := versionPkg.Version(dver.DockerVersion).LessThan("1.8.3")

	if useFallback {
		var v1Image image.V1Image
		err := json.Unmarshal(imageJSON, &v1Image)
		if err != nil {
			return nil, err
		}
		imageJSON, err = json.Marshal(v1Image)
		if err != nil {
			return nil, err
		}
	}

	var c map[string]*json.RawMessage
	if err := json.Unmarshal(imageJSON, &c); err != nil {
		return nil, err
	}

	c["rootfs"] = rawJSON(rootfs)
	c["history"] = rawJSON(history)

	return c, nil
}

func rawJSON(value interface{}) *json.RawMessage {
	jsonval, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return (*json.RawMessage)(&jsonval)
}
