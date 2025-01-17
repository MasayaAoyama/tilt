package tiltfile

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/builder/dockerignore"
	"github.com/pkg/errors"
	"go.starlark.net/starlark"

	"github.com/tilt-dev/tilt/internal/container"
	"github.com/tilt-dev/tilt/internal/dockerfile"
	"github.com/tilt-dev/tilt/internal/ospath"
	"github.com/tilt-dev/tilt/internal/sliceutils"
	"github.com/tilt-dev/tilt/internal/tiltfile/io"
	"github.com/tilt-dev/tilt/internal/tiltfile/starkit"
	"github.com/tilt-dev/tilt/internal/tiltfile/value"
	"github.com/tilt-dev/tilt/pkg/model"
)

var cacheObsoleteWarning = "docker_build(cache=...) is obsolete, and currently a no-op.\n" +
	"You should switch to live_update to optimize your builds."

type dockerImage struct {
	workDir          string
	configurationRef container.RefSelector
	matchInEnvVars   bool
	sshSpecs         []string
	secretSpecs      []string
	ignores          []string
	onlys            []string
	entrypoint       model.Cmd // optional: if specified, we override the image entrypoint/k8s command with this
	targetStage      string    // optional: if specified, we build a particular target in the dockerfile
	network          string
	extraTags        []string // Extra tags added at build-time.
	cacheFrom        []string
	pullParent       bool

	// Overrides the container args. Used as an escape hatch in case people want the old entrypoint behavior.
	// See discussion here:
	// https://github.com/tilt-dev/tilt/pull/2933
	containerArgs model.OverrideArgs

	dbDockerfilePath string
	dbDockerfile     dockerfile.Dockerfile
	dbBuildPath      string
	dbBuildArgs      model.DockerBuildArgs
	customCommand    model.Cmd
	customDeps       []string
	customTag        string

	// Whether this has been matched up yet to a deploy resource.
	matched bool

	dependencyIDs []model.TargetID

	// Only applicable to custom_build
	disablePush       bool
	skipsLocalDocker  bool
	outputsImageRefTo string

	liveUpdate model.LiveUpdate
}

func (d *dockerImage) ID() model.TargetID {
	return model.ImageID(d.configurationRef)
}

type dockerImageBuildType int

const (
	UnknownBuild = iota
	DockerBuild
	CustomBuild
)

func (d *dockerImage) Type() dockerImageBuildType {
	if d.dbBuildPath != "" {
		return DockerBuild
	}

	if !d.customCommand.Empty() {
		return CustomBuild
	}

	return UnknownBuild
}

func (s *tiltfileState) dockerBuild(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var dockerRef, targetStage string
	var contextVal,
		dockerfilePathVal,
		dockerfileContentsVal,
		cacheVal,
		liveUpdateVal,
		ignoreVal,
		onlyVal,
		entrypoint starlark.Value
	var buildArgs value.StringStringMap
	var network value.Stringable
	var ssh, secret, extraTags, cacheFrom value.StringOrStringList
	var matchInEnvVars, pullParent bool
	var containerArgsVal starlark.Sequence
	if err := s.unpackArgs(fn.Name(), args, kwargs,
		"ref", &dockerRef,
		"context", &contextVal,
		"build_args?", &buildArgs,
		"dockerfile?", &dockerfilePathVal,
		"dockerfile_contents?", &dockerfileContentsVal,
		"cache?", &cacheVal,
		"live_update?", &liveUpdateVal,
		"match_in_env_vars?", &matchInEnvVars,
		"ignore?", &ignoreVal,
		"only?", &onlyVal,
		"entrypoint?", &entrypoint,
		"container_args?", &containerArgsVal,
		"target?", &targetStage,
		"ssh?", &ssh,
		"secret?", &secret,
		"network?", &network,
		"extra_tag?", &extraTags,
		"cache_from?", &cacheFrom,
		"pull?", &pullParent,
	); err != nil {
		return nil, err
	}

	ref, err := container.ParseNamed(dockerRef)
	if err != nil {
		return nil, fmt.Errorf("Argument 1 (ref): can't parse %q: %v", dockerRef, err)
	}

	if contextVal == nil {
		return nil, fmt.Errorf("Argument 2 (context): empty but is required")
	}
	context, err := value.ValueToAbsPath(thread, contextVal)
	if err != nil {
		return nil, err
	}

	dockerfilePath := filepath.Join(context, "Dockerfile")
	var dockerfileContents string
	if dockerfileContentsVal != nil && dockerfilePathVal != nil {
		return nil, fmt.Errorf("Cannot specify both dockerfile and dockerfile_contents keyword arguments")
	}
	if dockerfileContentsVal != nil {
		switch v := dockerfileContentsVal.(type) {
		case io.Blob:
			dockerfileContents = v.Text
		case starlark.String:
			dockerfileContents = v.GoString()
		default:
			return nil, fmt.Errorf("Argument (dockerfile_contents): must be string or blob.")
		}
	} else if dockerfilePathVal != nil {
		dockerfilePath, err = value.ValueToAbsPath(thread, dockerfilePathVal)
		if err != nil {
			return nil, err
		}

		bs, err := io.ReadFile(thread, dockerfilePath)
		if err != nil {
			return nil, errors.Wrap(err, "error reading dockerfile")
		}
		dockerfileContents = string(bs)
	} else {
		bs, err := io.ReadFile(thread, dockerfilePath)
		if err != nil {
			return nil, errors.Wrapf(err, "error reading dockerfile")
		}
		dockerfileContents = string(bs)
	}

	if cacheVal != nil {
		s.logger.Warnf("%s", cacheObsoleteWarning)
	}

	liveUpdate, err := s.liveUpdateFromSteps(thread, liveUpdateVal)
	if err != nil {
		return nil, errors.Wrap(err, "live_update")
	}

	ignores, err := parseValuesToStrings(ignoreVal, "ignore")
	if err != nil {
		return nil, err
	}

	onlys, err := s.parseOnly(onlyVal)
	if err != nil {
		return nil, err
	}

	entrypointCmd, err := value.ValueToUnixCmd(thread, entrypoint, nil)
	if err != nil {
		return nil, err
	}

	var containerArgs model.OverrideArgs
	if containerArgsVal != nil {
		args, err := value.SequenceToStringSlice(containerArgsVal)
		if err != nil {
			return nil, fmt.Errorf("Argument 'container_args': %v", err)
		}
		containerArgs = model.OverrideArgs{ShouldOverride: true, Args: args}
	}

	for _, extraTag := range extraTags.Values {
		_, err := container.ParseNamed(extraTag)
		if err != nil {
			return nil, fmt.Errorf("Argument extra_tag=%q not a valid image reference: %v", extraTag, err)
		}
	}

	r := &dockerImage{
		workDir:          starkit.CurrentExecPath(thread),
		dbDockerfilePath: dockerfilePath,
		dbDockerfile:     dockerfile.Dockerfile(dockerfileContents),
		dbBuildPath:      context,
		configurationRef: container.NewRefSelector(ref),
		dbBuildArgs:      buildArgs.AsMap(),
		liveUpdate:       liveUpdate,
		matchInEnvVars:   matchInEnvVars,
		sshSpecs:         ssh.Values,
		secretSpecs:      secret.Values,
		ignores:          ignores,
		onlys:            onlys,
		entrypoint:       entrypointCmd,
		containerArgs:    containerArgs,
		targetStage:      targetStage,
		network:          network.Value,
		extraTags:        extraTags.Values,
		cacheFrom:        cacheFrom.Values,
		pullParent:       pullParent,
	}
	err = s.buildIndex.addImage(r)
	if err != nil {
		return nil, err
	}

	return starlark.None, nil
}

func (s *tiltfileState) parseOnly(val starlark.Value) ([]string, error) {
	paths, err := parseValuesToStrings(val, "only")
	if err != nil {
		return nil, err
	}

	for _, p := range paths {
		// We want to forbid file globs due to these issues:
		// https://github.com/tilt-dev/tilt/issues/1982
		// https://github.com/moby/moby/issues/30018
		if strings.Contains(p, "*") {
			return nil, fmt.Errorf("'only' does not support '*' file globs. Must be a real path: %s", p)
		}
	}
	return paths, nil
}

func (s *tiltfileState) customBuild(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var dockerRef string
	var commandVal, commandBat, commandBatVal starlark.Value
	deps := value.NewLocalPathListUnpacker(thread)
	var tag string
	var disablePush bool
	var liveUpdateVal, ignoreVal starlark.Value
	var matchInEnvVars bool
	var entrypoint starlark.Value
	var containerArgsVal starlark.Sequence
	var skipsLocalDocker bool
	outputsImageRefTo := value.NewLocalPathUnpacker(thread)

	err := s.unpackArgs(fn.Name(), args, kwargs,
		"ref", &dockerRef,
		"command", &commandVal,
		"deps", &deps,
		"tag?", &tag,
		"disable_push?", &disablePush,
		"skips_local_docker?", &skipsLocalDocker,
		"live_update?", &liveUpdateVal,
		"match_in_env_vars?", &matchInEnvVars,
		"ignore?", &ignoreVal,
		"entrypoint?", &entrypoint,
		"container_args?", &containerArgsVal,
		"command_bat_val", &commandBatVal,
		"outputs_image_ref_to", &outputsImageRefTo,

		// This is a crappy fix for https://github.com/tilt-dev/tilt/issues/4061
		// so that we don't break things.
		"command_bat", &commandBat,
	)
	if err != nil {
		return nil, err
	}

	ref, err := container.ParseNamed(dockerRef)
	if err != nil {
		return nil, fmt.Errorf("Argument 1 (ref): can't parse %q: %v", dockerRef, err)
	}

	liveUpdate, err := s.liveUpdateFromSteps(thread, liveUpdateVal)
	if err != nil {
		return nil, errors.Wrap(err, "live_update")
	}

	ignores, err := parseValuesToStrings(ignoreVal, "ignore")
	if err != nil {
		return nil, err
	}

	entrypointCmd, err := value.ValueToUnixCmd(thread, entrypoint, nil)
	if err != nil {
		return nil, err
	}

	var containerArgs model.OverrideArgs
	if containerArgsVal != nil {
		args, err := value.SequenceToStringSlice(containerArgsVal)
		if err != nil {
			return nil, fmt.Errorf("Argument 'container_args': %v", err)
		}
		containerArgs = model.OverrideArgs{ShouldOverride: true, Args: args}
	}

	if commandBat == nil {
		commandBat = commandBatVal
	}

	command, err := value.ValueGroupToCmdHelper(thread, commandVal, commandBat, nil)
	if err != nil {
		return nil, fmt.Errorf("Argument 2 (command): %v", err)
	} else if command.Empty() {
		return nil, fmt.Errorf("Argument 2 (command) can't be empty")
	}

	if tag != "" && outputsImageRefTo.Value != "" {
		return nil, fmt.Errorf("Cannot specify both tag= and outputs_image_ref_to=")
	}

	img := &dockerImage{
		workDir:           starkit.AbsWorkingDir(thread),
		configurationRef:  container.NewRefSelector(ref),
		customCommand:     command,
		customDeps:        deps.Value,
		customTag:         tag,
		disablePush:       disablePush,
		skipsLocalDocker:  skipsLocalDocker,
		liveUpdate:        liveUpdate,
		matchInEnvVars:    matchInEnvVars,
		ignores:           ignores,
		entrypoint:        entrypointCmd,
		containerArgs:     containerArgs,
		outputsImageRefTo: outputsImageRefTo.Value,
	}

	err = s.buildIndex.addImage(img)
	if err != nil {
		return nil, err
	}

	return &customBuild{s: s, img: img}, nil
}

type customBuild struct {
	s   *tiltfileState
	img *dockerImage
}

var _ starlark.Value = &customBuild{}

func (b *customBuild) String() string {
	return fmt.Sprintf("custom_build(%q)", b.img.configurationRef.String())
}

func (b *customBuild) Type() string {
	return "custom_build"
}

func (b *customBuild) Freeze() {}

func (b *customBuild) Truth() starlark.Bool {
	return true
}

func (b *customBuild) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: custom_build")
}

func (b *customBuild) AttrNames() []string {
	return []string{}
}

func parseValuesToStrings(value starlark.Value, param string) ([]string, error) {

	tempIgnores := starlarkValueOrSequenceToSlice(value)
	var ignores []string
	for _, v := range tempIgnores {
		switch val := v.(type) {
		case starlark.String: // for singular string
			goString := val.GoString()
			if strings.Contains(goString, "\n") {
				return nil, fmt.Errorf(param+" cannot contain newlines; found "+param+": %q", goString)
			}
			ignores = append(ignores, val.GoString())
		default:
			return nil, fmt.Errorf(param+" must be a string or a sequence of strings; found a %T", val)
		}
	}
	return ignores, nil

}

func isGitRepoBase(path string) bool {
	return ospath.IsDir(filepath.Join(path, ".git"))
}

func reposForPaths(paths []string) []model.LocalGitRepo {
	var result []model.LocalGitRepo
	repoSet := map[string]bool{}

	for _, path := range paths {
		isRepoBase := isGitRepoBase(path)
		if !isRepoBase || repoSet[path] {
			continue
		}

		repoSet[path] = true
		result = append(result, model.LocalGitRepo{
			LocalPath: path,
		})
	}

	return result
}

func (s *tiltfileState) reposForImage(image *dockerImage) []model.LocalGitRepo {
	var paths []string
	paths = append(paths,
		image.dbDockerfilePath,
		image.dbBuildPath,
		image.workDir)
	paths = append(paths, image.customDeps...)

	return reposForPaths(paths)
}

func (s *tiltfileState) defaultRegistry(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if !s.defaultReg.Empty() {
		return starlark.None, errors.New("default registry already defined")
	}

	var host, hostFromCluster, singleName string
	if err := s.unpackArgs(fn.Name(), args, kwargs,
		"host", &host,
		"host_from_cluster?", &hostFromCluster,
		"single_name?", &singleName); err != nil {
		return nil, err
	}

	reg, err := container.NewRegistryWithHostFromCluster(host, hostFromCluster)
	if err != nil {
		return starlark.None, errors.Wrapf(err, "validating defaultRegistry")
	}

	reg.SingleName = singleName

	s.defaultReg = reg

	return starlark.None, nil
}

func (s *tiltfileState) dockerignoresFromPathsAndContextFilters(source string, paths []string, ignorePatterns []string, onlys []string, dbDockerfilePath string) ([]model.Dockerignore, error) {
	var result []model.Dockerignore
	dupeSet := map[string]bool{}
	onlyPatterns := onlysToDockerignorePatterns(onlys)

	for _, path := range paths {
		if path == "" || dupeSet[path] {
			continue
		}
		dupeSet[path] = true

		if !ospath.IsDir(path) {
			continue
		}

		if len(ignorePatterns) != 0 {
			result = append(result, model.Dockerignore{
				LocalPath: path,
				Source:    source + " ignores=",
				Patterns:  ignorePatterns,
			})
		}

		if len(onlyPatterns) != 0 {
			result = append(result, model.Dockerignore{
				LocalPath: path,
				Source:    source + " only=",
				Patterns:  onlyPatterns,
			})
		}

		diFile := filepath.Join(path, ".dockerignore")
		customDiFile := dbDockerfilePath + ".dockerignore"
		_, err := os.Stat(customDiFile)
		if !os.IsNotExist(err) {
			diFile = customDiFile
		}

		s.postExecReadFiles = sliceutils.AppendWithoutDupes(s.postExecReadFiles, diFile)

		contents, err := ioutil.ReadFile(diFile)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}

		patterns, err := dockerignore.ReadAll(bytes.NewBuffer(contents))
		if err != nil {
			return nil, err
		}

		result = append(result, model.Dockerignore{
			LocalPath: path,
			Source:    diFile,
			Patterns:  patterns,
		})
	}

	return result, nil
}

func onlysToDockerignorePatterns(onlys []string) []string {
	if len(onlys) == 0 {
		return nil
	}

	result := []string{"**"}

	for _, only := range onlys {
		result = append(result, fmt.Sprintf("!%s", only))
	}

	return result
}

func (s *tiltfileState) dockerignoresForImage(image *dockerImage) ([]model.Dockerignore, error) {
	var paths []string
	var source string
	ref := image.configurationRef.RefFamiliarString()
	switch image.Type() {
	case DockerBuild:
		paths = append(paths, image.dbBuildPath)
		source = fmt.Sprintf("docker_build(%q)", ref)
	case CustomBuild:
		paths = append(paths, image.customDeps...)
		source = fmt.Sprintf("custom_build(%q)", ref)
	}
	return s.dockerignoresFromPathsAndContextFilters(
		source,
		paths, image.ignores, image.onlys, image.dbDockerfilePath)
}
