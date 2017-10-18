package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"

	"golang.org/x/tools/go/vcs"

	"github.com/BurntSushi/toml"
)

// Lock represents the parsed Gopkg.toml file.
type Lock struct {
	Projects []LockedProject `toml:"projects"`
}

// LockedProject represents one locked project, parsed from the Gopkg.toml file.
type LockedProject struct {
	Name     string   `toml:"name"`
	Branch   string   `toml:"branch,omitempty"`
	Revision string   `toml:"revision"`
	Version  string   `toml:"version,omitempty"`
	Source   string   `toml:"source,omitempty"`
	Packages []string `toml:"packages"`
}

type RemoteTarball struct {
	url         string
	stripPrefix string
	sha256      string
}

type RemoteGitRepo struct {
	revision string
}

type RemoteRepository interface {
	GetRepoString(name string, importPath string) string
}

func downloadFile(f *os.File, url string) (err error) {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

// github.com/scele/dep2bazel => com_github_scele_dep2bazel
func bazelName(importpath string) string {
	parts := strings.Split(importpath, "/")
	hostparts := strings.Split(parts[0], ".")
	var slice []string
	for i := len(hostparts) - 1; i >= 0; i-- {
		slice = append(slice, hostparts[i])
	}
	slice = append(slice, parts[1:]...)
	name := strings.Join(slice, "_")
	return strings.NewReplacer("-", "_", ".", "_").Replace(name)
}

func githubTarball(url string, revision string) (*RemoteTarball, error) {

	tarball := fmt.Sprintf("%v.tar.gz", revision)
	f, err := ioutil.TempFile("", "")
	if err != nil {
		return nil, err
	}
	filename := f.Name()
	defer os.Remove(filename)

	downloadURL := fmt.Sprintf("%v/archive/%v", url, tarball)
	err = downloadFile(f, downloadURL)
	if err != nil {
		return nil, err
	}
	f.Close()

	// Github tarballs have one top-level directory that we want to strip out.
	// Determine the name of that directory by inspecting the tarball.
	// Usually the directory name is just importname-revision, but we can't assume
	// it since capitalization might differ.
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	gzf, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	tarReader := tar.NewReader(gzf)

	// The root directory is the second entry in the tarball.
	head, err := tarReader.Next()
	if err != nil {
		return nil, err
	}
	head, err = tarReader.Next()
	if err != nil {
		return nil, err
	}
	stripPrefix := head.Name

	// Also compute checksum for the downloaded file.
	sha := fmt.Sprintf("%x", sha256.Sum256(b))

	return &RemoteTarball{
		url:         downloadURL,
		stripPrefix: stripPrefix,
		sha256:      sha,
	}, nil
}

func googlesourceTarball(url string, revision string) (*RemoteTarball, error) {
	return &RemoteTarball{
		url:         fmt.Sprintf("%v/+archive/%v.tar.gz", url, revision),
		stripPrefix: "",
		// Astonishingly, archives downloaded from go.googlesource.com produce
		// different checksum for each download...
		sha256: "",
	}, nil
}

var gopkgInPatternOld = regexp.MustCompile(`^/(?:([a-z0-9][-a-z0-9]+)/)?((?:v0|v[1-9][0-9]*)(?:\.0|\.[1-9][0-9]*){0,2}(?:-unstable)?)/([a-zA-Z][-a-zA-Z0-9]*)(?:\.git)?((?:/[a-zA-Z][-a-zA-Z0-9]*)*)$`)
var gopkgInPatternNew = regexp.MustCompile(`^/(?:([a-zA-Z0-9][-a-zA-Z0-9]+)/)?([a-zA-Z][-.a-zA-Z0-9]*)\.((?:v0|v[1-9][0-9]*)(?:\.0|\.[1-9][0-9]*){0,2}(?:-unstable)?)(?:\.git)?((?:/[a-zA-Z0-9][-.a-zA-Z0-9]*)*)$`)

// We map some urls to github urls, since github has good support for downloading
// tarball snapshots.
func remapURL(url string) string {
	if strings.HasPrefix(url, "https://gopkg.in/") {
		// Special handling for gopkg.in which does not support downloading tarballs.
		// Remap gopkg.in => github.com.
		tail := url[len("https://gopkg.in"):]
		m := gopkgInPatternNew.FindStringSubmatch(tail)
		if m == nil {
			m = gopkgInPatternOld.FindStringSubmatch(tail)
			if m == nil {
				return url
			}
			// "/v2/name" <= "/name.v2"
			m[2], m[3] = m[3], m[2]
		}
		repoUser := m[1]
		repoName := m[2]
		if repoUser != "" {
			return "https://github.com/" + repoUser + "/" + repoName
		}
		return "https://github.com/go-" + repoName + "/" + repoName
	} else if strings.HasPrefix(url, "https://go.googlesource.com/") {
		// Try github mirror because go.googlesource.com does not give deterministic
		// checksums for tarball downloads.
		_, repoName := path.Split(url)
		return "https://github.com/golang/" + repoName
	}
	return url
}

func tryTarball(url string, revision string) (*RemoteTarball, error) {
	if strings.HasPrefix(url, "https://github.com/") {
		return githubTarball(url, revision)
	} else if strings.HasPrefix(url, "https://go.googlesource.com/") {
		return googlesourceTarball(url, revision)
	} else {
		return &RemoteTarball{}, fmt.Errorf("Unknown server")
	}
}

// GetRepoString returns the go_repository rule string.
func (t *RemoteTarball) GetRepoString(name string, importPath string) string {
	str := fmt.Sprintf("\n")
	str += fmt.Sprintf("    go_repository(\n")
	str += fmt.Sprintf("        name = \"%v\",\n", name)
	str += fmt.Sprintf("        importpath = \"%v\",\n", importPath)
	str += fmt.Sprintf("        urls = [\"%v\"],\n", t.url)
	str += fmt.Sprintf("        strip_prefix = \"%v\",\n", t.stripPrefix)
	if t.sha256 != "" {
		str += fmt.Sprintf("        sha256 = \"%v\",\n", t.sha256)
	}
	str += fmt.Sprintf("        build_file_proto_mode = \"disable\",\n")
	str += fmt.Sprintf("    )\n")
	return str
}

// GetRepoString returns the go_repository rule string.
func (t *RemoteGitRepo) GetRepoString(name string, importPath string) string {
	str := fmt.Sprintf("\n")
	str += fmt.Sprintf("    go_repository(\n")
	str += fmt.Sprintf("        name = \"%v\",\n", name)
	str += fmt.Sprintf("        importpath = \"%v\",\n", importPath)
	str += fmt.Sprintf("        commit = \"%v\",\n", t.revision)
	str += fmt.Sprintf("        build_file_proto_mode = \"disable\",\n")
	str += fmt.Sprintf("    )\n")
	return str
}

func remoteRepository(url string, importName string, revision string) (RemoteRepository, error) {

	remappedURL := remapURL(url)

	// First, try downloading a tarball using our remapped url.
	tarball, err := tryTarball(remappedURL, revision)
	if err == nil {
		return tarball, nil
	}

	// Then, try downloading a tarball using the original url.
	tarball, err = tryTarball(url, revision)
	if err == nil {
		return tarball, nil
	}

	// If downloading a tarball failed, default to downloading with git.
	return &RemoteGitRepo{revision: revision}, nil
}

const repoTemplateNoChecksum = `
    go_repository(
        name = "%v",
        importpath = "%v",
        urls = ["%v"],
        strip_prefix = "%v",
        build_file_proto_mode = "disable",
    )
`

func usage() {
	fmt.Println("usage: dep2bazel path/to/Gopkg.lock")
	os.Exit(1)
}

func main() {
	if len(os.Args) != 2 {
		usage()
	}

	filename := strings.TrimSpace(os.Args[1])
	if filename == "" {
		usage()
	}

	content, err := ioutil.ReadFile(filename)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to read Gopkg.lock", err)
		os.Exit(1)
	}

	raw := Lock{}
	err = toml.Unmarshal(content, &raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to parse Gopkg.lock", err)
		os.Exit(1)
	}

	fmt.Printf(`# This file is autogenerated with dep2bazel, do not edit.
load("@io_bazel_rules_go//go:def.bzl", "go_repository")

def go_deps():
`)

	for _, lp := range raw.Projects {
		root, err := vcs.RepoRootForImportPath(lp.Name, false)
		if err != nil {
			fmt.Println(err)
			continue
		}
		importpath := lp.Name
		repo, err := remoteRepository(root.Repo, lp.Name, lp.Revision)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to parse %v (%v@%v): %v\n", lp.Name, root.Repo, lp.Revision, err)
		} else {
			fmt.Print(repo.GetRepoString(bazelName(importpath), importpath))
		}
	}

}
