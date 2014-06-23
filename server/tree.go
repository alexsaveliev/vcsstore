package server

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sort"

	"github.com/sourcegraph/go-vcs/vcs"
	"github.com/sourcegraph/vcsstore/vcsclient"
	"github.com/sqs/mux"
)

func (h *Handler) serveRepoTreeEntry(w http.ResponseWriter, r *http.Request) error {
	v := mux.Vars(r)

	repo, _, _, err := h.getRepo(r, 0)
	if err != nil {
		return err
	}

	commitID, canon, err := getCommitID(r)
	if err != nil {
		return err
	}

	type fileSystem interface {
		FileSystem(vcs.CommitID) (vcs.FileSystem, error)
	}
	if repo, ok := repo.(fileSystem); ok {
		fs, err := repo.FileSystem(commitID)
		if err != nil {
			return err
		}

		path := v["Path"]
		fi, err := fs.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return &httpError{http.StatusNotFound, err}
			}
			return err
		}

		e := newTreeEntry(fi)

		if fi.Mode().IsDir() {
			entries, err := fs.ReadDir(path)
			if err != nil {
				return err
			}

			e.Entries = make([]*vcsclient.TreeEntry, len(entries))
			for i, fi := range entries {
				e.Entries[i] = newTreeEntry(fi)
			}
			sort.Sort(vcsclient.TreeEntriesByTypeByName(e.Entries))
		} else if fi.Mode().IsRegular() {
			f, err := fs.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()

			contents, err := ioutil.ReadAll(f)
			if err != nil {
				return err
			}

			e.Contents = contents
		}

		if canon {
			setLongCache(w)
		} else {
			setShortCache(w)
		}
		return writeJSON(w, e)
	}

	return &httpError{http.StatusNotImplemented, fmt.Errorf("FileSystem not yet implemented for %T", repo)}
}

func newTreeEntry(fi os.FileInfo) *vcsclient.TreeEntry {
	e := &vcsclient.TreeEntry{
		Name:    fi.Name(),
		Size:    int(fi.Size()),
		ModTime: fi.ModTime(),
	}
	if fi.Mode().IsDir() {
		e.Type = vcsclient.DirEntry
	} else if fi.Mode().IsRegular() {
		e.Type = vcsclient.FileEntry
	} else if fi.Mode()&os.ModeSymlink != 0 {
		e.Type = vcsclient.SymlinkEntry
	}
	return e
}
