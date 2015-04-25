package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"sourcegraph.com/sourcegraph/go-vcs/vcs"
	"sourcegraph.com/sourcegraph/vcsstore/vcsclient"
)

func TestServeRepoCommits(t *testing.T) {
	setupHandlerTest()
	defer teardownHandlerTest()

	repoPath := "a.b/c"
	opt := vcs.CommitsOptions{Head: "abcd", N: 2, Skip: 3}

	rm := &mockCommits{
		t:       t,
		opt:     opt,
		commits: []*vcs.Commit{{ID: "abcd"}, {ID: "wxyz"}},
		total:   123,
	}
	sm := &mockServiceForExistingRepo{
		t:        t,
		repoPath: repoPath,
		repo:     rm,
	}
	testHandler.Service = sm

	resp, err := http.Get(server.URL + testHandler.router.URLToRepoCommits(repoPath, opt).String())
	if err != nil && !isIgnoredRedirectErr(err) {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !sm.opened {
		t.Errorf("!opened")
	}
	if !rm.called {
		t.Errorf("!called")
	}

	if total, want := resp.Header.Get(vcsclient.TotalCommitsHeader), "123"; total != want {
		t.Errorf("got total commits header %q, want %q", total, want)
	}

	var commits []*vcs.Commit
	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(commits, rm.commits) {
		t.Errorf("got commits %+v, want %+v", commits, rm.commits)
	}
}

type mockCommits struct {
	t *testing.T

	// expected args
	opt vcs.CommitsOptions

	// return values
	commits []*vcs.Commit
	total   uint
	err     error

	called bool
}

func (m *mockCommits) Commits(opt vcs.CommitsOptions) ([]*vcs.Commit, uint, error) {
	if opt != m.opt {
		m.t.Errorf("mock: got opt %+v, want %+v", opt, m.opt)
	}
	m.called = true
	return m.commits, m.total, m.err
}
