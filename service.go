package vcsstore

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sourcegraph.com/sourcegraph/go-vcs/vcs"
	"sourcegraph.com/sourcegraph/vcsstore/vcsclient"
)

type Service interface {
	// Open opens a repository. If it doesn't exist. an
	// os.ErrNotExist-satisfying error is returned. If opening succeeds, the
	// repository is returned.
	Open(repoPath string) (interface{}, error)

	// Close closes the repository.
	Close(repoPath string)

	// Clone clones the repository if a clone doesn't yet exist locally.
	// Otherwise, it opens the repository. If no errors occur, the repository is
	// returned.
	Clone(repoPath string, cloneInfo *vcsclient.CloneInfo) (interface{}, error)
}

type Config struct {
	// StorageDir is where cloned repositories are stored. If empty, the current
	// working directory is used.
	StorageDir string

	Log *log.Logger

	DebugLog *log.Logger
}

// CloneDir validates vcsType and cloneURL. If they are valid, cloneDir returns
// the local directory that the repository should be cloned to (which it may
// already exist at). If invalid, cloneDir returns a non-nil error.
func (c *Config) CloneDir(repoPath string) (string, error) {
	return filepath.Join(c.StorageDir, EncodeRepositoryPath(repoPath)), nil
}

func NewService(c *Config) Service {
	if c == nil {
		c = &Config{
			StorageDir: ".",
			Log:        log.New(os.Stderr, "vcsstore: ", log.LstdFlags),
			DebugLog:   log.New(ioutil.Discard, "", 0),
		}
	}
	return &service{
		Config:    *c,
		repoMu:    make(map[repoKey]*sync.RWMutex),
		repos:     map[repoKey]interface{}{},
		repoUsers: map[repoKey]int{},
	}
}

type service struct {
	Config

	// repoMu prevents more than one goroutine from simultaneously
	// cloning the same repository.
	repoMu map[repoKey]*sync.RWMutex

	// repo and repoUsers holds all repos that have been opened and not yet
	// closed. When the count goes to 0, the repo can be freed. It is
	// protected by repoMuMu.
	repos     map[repoKey]interface{}
	repoUsers map[repoKey]int

	// repoMuMu synchronizes access to repoMu, repo, and repoUsers.
	repoMuMu sync.RWMutex
}

type repoKey struct {
	cloneDir string
}

func (s *service) Open(repoPath string) (interface{}, error) {
	cloneDir, err := s.CloneDir(repoPath)
	if err != nil {
		return nil, err
	}
	return s.open(cloneDir)
}

func (s *service) open(cloneDir string) (interface{}, error) {
	key := repoKey{cloneDir}
	vcsType, err := vcsTypeFromDir(cloneDir)
	if err != nil {
		return nil, err
	}

	// Quick check if another goroutine has already opened (and not
	// yet closed) the repo. Use that instance if so.
	s.repoMuMu.Lock()
	if repo := s.repos[key]; repo != nil {
		s.repoMuMu.Unlock()
		return repo, nil
	}
	s.repoMuMu.Unlock()

	if fi, err := os.Stat(cloneDir); err != nil {
		return nil, err
	} else if !fi.Mode().IsDir() {
		return nil, fmt.Errorf("clone path %q is not a directory", cloneDir)
	}
	repo, err := vcs.Open(vcsType, cloneDir)
	if err != nil {
		return nil, err
	}

	s.repoMuMu.Lock()
	defer s.repoMuMu.Unlock()
	s.repoUsers[key]++
	if repo := s.repos[key]; repo != nil {
		// Another goroutine raced us to open this repo. Use ours, not
		// theirs, so that there is only 1 instance of this repo in
		// use at a time.
		return repo, nil
	}
	// Otherwise, tell other goroutines to use the repo we just opened.
	s.repos[key] = repo

	return repo, nil
}

func (s *service) Close(repoPath string) {
	cloneDir, err := s.CloneDir(repoPath)
	if err != nil {
		panic(err)
	}
	s.repoMuMu.Lock()
	defer s.repoMuMu.Unlock()
	key := repoKey{cloneDir}
	s.repoUsers[key]--
	if s.repoUsers[key] == 0 {
		delete(s.repoUsers, key)
		delete(s.repos, key)
	}
}

func (s *service) Clone(repoPath string, cloneInfo *vcsclient.CloneInfo) (interface{}, error) {
	cloneDir, err := s.CloneDir(repoPath)
	if err != nil {
		return nil, err
	}

	// See if the clone directory exists and return immediately (without
	// locking) if so.
	if r, err := s.open(repoPath); !os.IsNotExist(err) {
		if err == nil {
			s.debugLogf("Clone(%s): repository already exists at %s", repoPath, cloneDir)
		} else {
			s.debugLogf("Clone(%s): opening existing repository at %s failed: %s", repoPath, cloneDir, err)
		}
		return r, err
	}

	// The local clone directory doesn't exist, so we need to clone the repository.
	mu := s.Mutex(repoKey{cloneDir})
	mu.Lock()
	defer mu.Unlock()

	// Check again after obtaining the lock, so we don't clone multiple times.
	if r, err := s.open(cloneDir); !os.IsNotExist(err) {
		if err == nil {
			s.debugLogf("Clone(%s): after obtaining clone lock, repository already exists at %s", repoPath, cloneDir)
		} else {
			s.debugLogf("Clone(%s): after obtaining clone lock, opening existing repository at %s failed: %s", repoPath, cloneDir, err)
		}
		return r, err
	}

	start := time.Now()
	msg := fmt.Sprintf("%s to %s", repoPath, cloneDir)
	s.Log.Print("Cloning ", msg, "...")

	// "Atomically" clone the repository. First, clone it to a temporary sibling
	// directory. Once the clone is complete, "atomically"
	// rename it to the intended cloneDir.
	//
	// "Atomically" is in quotes because this operation is not really atomic. It
	// depends on the underlying FS. For now, for our purposes, it performs well
	// enough on local ext4 and on GlusterFS.
	parentDir := filepath.Dir(cloneDir)
	if err := os.MkdirAll(parentDir, 0700); err != nil {
		return nil, err
	}

	cloneTmpDir, err := ioutil.TempDir(parentDir, "_tmp_"+filepath.Base(cloneDir)+"-")
	if err != nil {
		return nil, err
	}
	s.debugLogf("Clone(%s, %s): cloning to temporary sibling dir %s", repoPath, cloneTmpDir)
	defer os.RemoveAll(cloneTmpDir)

	cloneOpt := vcs.CloneOpt{Bare: true, Mirror: true, RemoteOpts: cloneInfo.RemoteOpts}
	_, err = vcs.Clone(cloneInfo.VCS, cloneInfo.CloneURL, cloneTmpDir, cloneOpt)
	if err != nil {
		return nil, err
	}
	s.debugLogf("Clone(%s, %s): cloned to temporary sibling dir %s; now renaming to intended clone dir %s", cloneInfo.VCS, cloneInfo.CloneURL, cloneTmpDir, cloneDir)

	if err := os.Rename(cloneTmpDir, cloneDir); err != nil {
		s.debugLogf("Clone(%s, %s): Rename(%s -> %s) failed: %s", cloneInfo.VCS, cloneInfo.CloneURL, cloneTmpDir, cloneDir)
		return nil, err
	}

	defer func() {
		s.Log.Print("Finished cloning ", msg, " in ", time.Since(start))
	}()

	return s.open(cloneDir)
}

func (s *service) Mutex(key repoKey) *sync.RWMutex {
	s.repoMuMu.Lock()
	defer s.repoMuMu.Unlock()

	if mu, ok := s.repoMu[key]; ok {
		return mu
	}
	s.repoMu[key] = &sync.RWMutex{}
	return s.repoMu[key]
}

func isLowercaseLetter(s string) bool {
	return strings.IndexFunc(s, func(c rune) bool {
		return !(c >= 'a' && c <= 'z')
	}) == -1
}

func (s *service) debugLogf(format string, args ...interface{}) {
	if s.DebugLog != nil {
		s.DebugLog.Printf(format, args...)
	}
}
