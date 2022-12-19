package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

//nolint:gocritic // regexpSimplify: [0-9] reads better here than \d
var versionRE = regexp.MustCompile(`^1(\.[1-9][0-9]*)?(\.[1-9][0-9]*)?((rc|beta)[1-9]+)?$`)

// use switches the current Go version to the one specified.
// If it's not installed, use will install it and download its SDK first.
func use(ctx context.Context, args []string, gobin, sdk fsx) error {
	if len(args) == 0 {
		return usageError{errors.New("no version has been specified")}
	}

	local, err := localVersions(ctx, gobin)
	if err != nil {
		return err
	}

	version := args[0]
	if version == "main" {
		version = local.main
	}

	if !versionRE.MatchString(version) {
		return fmt.Errorf("malformed version %q", version)
	}

	switch version {
	case local.current:
		printf("%s is already in use\n", version)
		return nil
	case local.main:
		// for switching to the main version simply removing the symlink is enough.
		if err := gobin.Remove("go"); err != nil {
			return err
		}
		printf("Switched to %s (main)\n", version)
		return nil
	}

	initial := false
	if !local.contains(version) {
		initial = true
		printf("%s is not installed. Looking for it on go.dev ...\n", version)
		url := fmt.Sprintf("golang.org/dl/go%s@latest", version)
		if err := command(ctx, "go", "install", url); err != nil {
			return err
		}
	}

	// it's possible that SDK download was canceled during initial installation,
	// so we need to ensure its presence even if the go<version> binary exists.
	if !downloaded(version, sdk) {
		if !initial {
			// this message doesn't make sense during initial installation.
			printf("%s SDK is missing. Starting download ...\n", version)
		}
		if err := command(ctx, "go"+version, "download"); err != nil {
			return err
		}
	}

	// it's ok for the symlink to be missing if the previous version was the main one.
	if err := gobin.Remove("go"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := gobin.Symlink("go"+version, "go"); err != nil {
		return err
	}

	printf("Switched to %s\n", version)
	return nil
}

// list prints the list of installed Go versions, highlighting the current one.
// If the -all flag is provided, list prints available versions from go.dev as well.
func list(ctx context.Context, args []string, gobin, sdk fsx) error {
	fset := flag.NewFlagSet("list", flag.ContinueOnError)
	fset.SetOutput(io.Discard)

	var printAll bool
	fset.BoolVar(&printAll, "a", false, "shorthand for -all")
	fset.BoolVar(&printAll, "all", false, "print available versions from go.dev as well")

	var only string
	fset.StringVar(&only, "only", "", "print only versions starting with this prefix")

	if err := fset.Parse(args); err != nil {
		return usageError{err}
	}

	local, err := localVersions(ctx, gobin)
	if err != nil {
		return err
	}

	versions := local.list
	if printAll {
		if versions, err = remoteVersions(ctx); err != nil {
			return err
		}
	}

	for _, version := range versions {
		if !strings.HasPrefix(version, only) {
			continue
		}

		var extra string
		switch {
		case version == local.main:
			extra = " (main)"
		case !local.contains(version):
			extra = " (not installed)"
		case !downloaded(version, sdk):
			extra = " (missing SDK)"
		}

		prefix := " "
		if version == local.current {
			prefix = "*"
		}

		printf("%s %-10s%s\n", prefix, version, extra)
	}

	return nil
}

// remove removes the specified Go version (both the binary and the SDK).
// If this version is current, remove will switch to the main one first.
func remove(ctx context.Context, args []string, gobin, sdk fsx) error {
	if len(args) == 0 {
		return usageError{errors.New("no version has been specified")}
	}

	local, err := localVersions(ctx, gobin)
	if err != nil {
		return err
	}

	version := args[0]
	if version == "main" {
		version = local.main
	}

	if !versionRE.MatchString(version) {
		return fmt.Errorf("malformed version %q", version)
	}

	if !local.contains(version) {
		return fmt.Errorf("%s is not installed", version)
	}

	switch version {
	case local.main:
		return fmt.Errorf("unable to remove %s (main)", version)
	case local.current:
		// switch to the main version first.
		if err := gobin.Remove("go"); err != nil {
			return err
		}
		printf("Switched to %s (main)\n", local.main)
	}

	if err := gobin.Remove("go" + version); err != nil {
		return err
	}
	if err := sdk.RemoveAll("go" + version); err != nil {
		return err
	}

	printf("Removed %s\n", version)
	return nil
}

// downloaded checks whether the SDK of the specified Go version has been downloaded.
func downloaded(version string, sdk fs.FS) bool {
	// from https://github.com/golang/dl/blob/master/internal/version/version.go
	// .unpacked-success is a sentinel zero-byte file to indicate that the Go
	// version was downloaded and unpacked successfully.
	_, err := fs.Stat(sdk, "go"+version+"/.unpacked-success")
	return err == nil
}

type local struct {
	main    string
	current string
	list    []string // (includes both main and current).
}

func (l *local) contains(version string) bool {
	for _, v := range l.list {
		if v == version {
			return true
		}
	}
	return false
}

// localVersions returns the list of installed Go versions.
func localVersions(ctx context.Context, gobin fsx) (*local, error) {
	currPath := os.Getenv("PATH")
	defer os.Setenv("PATH", currPath)

	// to make exec.Command use the main go binary,
	// we need to temporary remove $GOBIN from $PATH.
	tempPath := cutFromPath(currPath, os.Getenv("GOBIN"))
	os.Setenv("PATH", tempPath)

	output, err := exec.CommandContext(ctx, "go", "version").Output()
	if err != nil {
		return nil, err
	}

	// the format is `go version go1.18 darwin/arm64`, we want the semver part.
	parts := strings.Split(string(output), " ")
	if len(parts) != 4 {
		return nil, fmt.Errorf("unexpected format %q", output)
	}

	main, current := strings.TrimPrefix(parts[2], "go"), ""

	target, err := gobin.Readlink("go")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		current = main // the main version is already in use.
	case err == nil:
		current = strings.TrimPrefix(filepath.Base(target), "go")
	default:
		return nil, err
	}

	entries, err := fs.ReadDir(gobin, ".")
	if err != nil {
		return nil, err
	}

	list := []string{main}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		version := strings.TrimPrefix(entry.Name(), "go")
		if versionRE.MatchString(version) {
			list = append(list, version)
		}
	}

	sort.Slice(list, func(i, j int) bool {
		return versionLess(list[i], list[j])
	})

	return &local{
		main:    main,
		current: current,
		list:    list,
	}, nil
}

// remoteVersions returns the list of all Go versions from go.dev.
func remoteVersions(ctx context.Context) ([]string, error) {
	const url = "https://go.dev/dl/?mode=json&include=all"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: time.Minute}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// sorted by version, from newest to oldest.
	var list []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}

	versions := make([]string, len(list))
	for i, v := range list {
		versions[i] = strings.TrimPrefix(v.Version, "go")
	}

	return versions, nil
}

// command is a wrapper for exec.Command that redirects stdout/stderr.
func command(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cutFromPath cuts the given value from a $PATH-like string.
func cutFromPath(path, value string) string {
	var list []string
	for _, v := range strings.Split(path, string(os.PathListSeparator)) {
		if v != value {
			list = append(list, v)
		}
	}
	return strings.Join(list, string(os.PathListSeparator))
}