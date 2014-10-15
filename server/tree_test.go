package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"code.google.com/p/go.tools/godoc/vfs"
	"code.google.com/p/go.tools/godoc/vfs/mapfs"
	"github.com/sourcegraph/go-vcs/vcs"
	"github.com/sourcegraph/vcsstore/vcsclient"
)

func TestServeRepoTreeEntry_File(t *testing.T) {
	setupHandlerTest()
	defer teardownHandlerTest()

	commitID := vcs.CommitID(strings.Repeat("a", 40))

	cloneURL, _ := url.Parse("git://a.b/c")
	rm := &mockFileSystem{
		t:  t,
		at: commitID,
		fs: mapfs.New(map[string]string{"myfile": "mydata"}),
	}
	sm := &mockServiceForExistingRepo{
		t:        t,
		vcs:      "git",
		cloneURL: cloneURL,
		repo:     rm,
	}
	testHandler.Service = sm

	resp, err := http.Get(server.URL + testHandler.router.URLToRepoTreeEntry("git", cloneURL, commitID, "myfile").String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Errorf("got status code %d, want %d", got, want)
	}

	if !sm.opened {
		t.Errorf("!opened")
	}
	if !rm.called {
		t.Errorf("!called")
	}

	var e *vcsclient.TreeEntry
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}

	wantEntry := &vcsclient.TreeEntry{
		Name:     "myfile",
		Type:     vcsclient.FileEntry,
		Size:     6,
		Contents: []byte("mydata"),
	}
	normalizeTreeEntry(wantEntry)

	if !reflect.DeepEqual(e, wantEntry) {
		t.Errorf("got tree entry %+v, want %+v", e, wantEntry)
	}

	// used canonical commit ID, so should be long-cached
	if cc := resp.Header.Get("cache-control"); cc != longCacheControl {
		t.Errorf("got cache-control %q, want %q", cc, longCacheControl)
	}
}

func TestServeRepoTreeEntry_Dir(t *testing.T) {
	setupHandlerTest()
	defer teardownHandlerTest()

	cloneURL, _ := url.Parse("git://a.b/c")
	rm := &mockFileSystem{
		t:  t,
		at: "abcd",
		fs: mapfs.New(map[string]string{"myfile": "mydata", "mydir/f": ""}),
	}
	sm := &mockServiceForExistingRepo{
		t:        t,
		vcs:      "git",
		cloneURL: cloneURL,
		repo:     rm,
	}
	testHandler.Service = sm

	resp, err := http.Get(server.URL + testHandler.router.URLToRepoTreeEntry("git", cloneURL, "abcd", ".").String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Errorf("got status code %d, want %d", got, want)
	}

	if !sm.opened {
		t.Errorf("!opened")
	}
	if !rm.called {
		t.Errorf("!called")
	}

	var e *vcsclient.TreeEntry
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}

	wantEntry := &vcsclient.TreeEntry{
		Name: ".",
		Type: vcsclient.DirEntry,
		Entries: []*vcsclient.TreeEntry{
			{
				Name: "mydir",
				Type: vcsclient.DirEntry,
			},
			{
				Name: "myfile",
				Type: vcsclient.FileEntry,
				Size: 6,
			},
		},
	}
	normalizeTreeEntry(wantEntry)

	if !reflect.DeepEqual(e, wantEntry) {
		t.Errorf("got tree entry %+v, want %+v", e, wantEntry)
	}

	// used short commit ID, so should not be long-cached
	if cc := resp.Header.Get("cache-control"); cc != shortCacheControl {
		t.Errorf("got cache-control %q, want %q", cc, shortCacheControl)
	}
}

func TestServeRepoTreeEntry_FileWithOptions(t *testing.T) {
	setupHandlerTest()
	defer teardownHandlerTest()

	commitID := vcs.CommitID(strings.Repeat("a", 40))

	cloneURL, _ := url.Parse("git://a.b/c")
	rm := &mockFileSystem{
		t:  t,
		at: commitID,
		fs: mapfs.New(map[string]string{"myfile": "mydata"}),
	}
	sm := &mockServiceForExistingRepo{
		t:        t,
		vcs:      "git",
		cloneURL: cloneURL,
		repo:     rm,
	}
	testHandler.Service = sm

	resp, err := http.Get(server.URL + testHandler.router.URLToRepoTreeEntry("git", cloneURL, commitID, "myfile").String() + "?StartByte=2&EndByte=4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Errorf("got status code %d, want %d", got, want)
	}

	if !sm.opened {
		t.Errorf("!opened")
	}
	if !rm.called {
		t.Errorf("!called")
	}

	var f *vcsclient.FileWithRange
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		t.Fatal(err)
	}

	want := &vcsclient.FileWithRange{
		TreeEntry: &vcsclient.TreeEntry{
			Name:     "myfile",
			Type:     vcsclient.FileEntry,
			Size:     6,
			Contents: []byte("da"),
		},
		FileRange: vcsclient.FileRange{
			StartByte: 2, EndByte: 4,
			StartLine: 1, EndLine: 1,
		},
	}
	normalizeTreeEntry(want.TreeEntry)

	if !reflect.DeepEqual(f, want) {
		t.Errorf("got file with range %+v, want %+v", f, want)
	}

	// used canonical commit ID, so should be long-cached
	if cc := resp.Header.Get("cache-control"); cc != longCacheControl {
		t.Errorf("got cache-control %q, want %q", cc, longCacheControl)
	}
}

type mockFileSystem struct {
	t *testing.T

	// expected args
	at vcs.CommitID

	// return values
	fs  vfs.FileSystem
	err error

	called bool
}

func (m *mockFileSystem) FileSystem(at vcs.CommitID) (vfs.FileSystem, error) {
	if at != m.at {
		m.t.Errorf("mock: got at arg %q, want %q", at, m.at)
	}
	m.called = true
	return m.fs, m.err
}

func normalizeTreeEntry(e *vcsclient.TreeEntry) {
	e.ModTime = e.ModTime.In(time.UTC)
	for _, e := range e.Entries {
		normalizeTreeEntry(e)
	}
}
