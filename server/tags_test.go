package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"sourcegraph.com/sourcegraph/go-vcs/vcs"
)

func TestServeRepoTags(t *testing.T) {
	setupHandlerTest()
	defer teardownHandlerTest()

	repoPath := "a.b/c"
	rm := &mockTags{
		t:    t,
		tags: []*vcs.Tag{{Name: "t", CommitID: "c"}},
	}
	sm := &mockServiceForExistingRepo{
		t:        t,
		repoPath: repoPath,
		repo:     rm,
	}
	testHandler.Service = sm

	resp, err := http.Get(server.URL + testHandler.router.URLToRepoTags(repoPath).String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !sm.opened {
		t.Errorf("!opened")
	}
	if !rm.called {
		t.Errorf("!called")
	}

	var tags []*vcs.Tag
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(tags, rm.tags) {
		t.Errorf("got tags %+v, want %+v", tags, rm.tags)
	}
}

type mockTags struct {
	t *testing.T

	// return values
	tags []*vcs.Tag
	err  error

	called bool
}

func (m *mockTags) Tags() ([]*vcs.Tag, error) {
	m.called = true
	return m.tags, m.err
}
