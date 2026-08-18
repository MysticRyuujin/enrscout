package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	xssh "golang.org/x/crypto/ssh"
)

// The default file transport shells out to git-upload-pack, which distroless-minded tests must not
// rely on; the pure-Go pack server keeps the whole round trip in-process.
func init() {
	client.InstallProtocol("file", server.NewClient(server.NewFilesystemLoader(osfs.New("/"))))
}

func newBareRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatal(err)
	}
	return "file://" + dir
}

func testGitPublisher(t *testing.T, url string) *gitPublisher {
	t.Helper()
	return &gitPublisher{
		repoURL: url, branch: "master", dir: t.TempDir(),
		name: "enrscout-test", email: "enrscout@test.invalid", timeout: time.Minute,
	}
}

func testBuiltTree(domain string, nodes int, seq uint64) builtTree {
	t := builtTree{
		output: output{
			SchemaVersion: outputSchemaVersion,
			URL:           "enrtree://KEY@" + domain,
			Domain:        domain, Network: "hoodi", Capability: "all",
			Nodes: nodes, Seq: seq,
			Records: map[string]string{domain: fmt.Sprintf("enrtree-root:v1 seq=%d", seq)},
		},
		signature: "c2ln",
	}
	for i := 0; i < nodes; i++ {
		t.nodes = append(t.nodes, publishedNode{
			id:     fmt.Sprintf("%064d", i),
			record: fmt.Sprintf("enr:-node%d", i),
			seq:    uint64(i + 1), score: int32(10 * (i + 1)),
			firstSeen: 1700000000, lastResolved: 1700003600, lastCheck: 1700003600,
		})
	}
	return t
}

func remoteFile(t *testing.T, url, path string) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainClone(dir, false, &git.CloneOptions{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	_ = head
	data, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		return "", false
	}
	return string(data), true
}

func remoteCommits(t *testing.T, url string) int {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainClone(dir, false, &git.CloneOptions{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	iter, err := repo.Log(&git.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := iter.ForEach(func(*object.Commit) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGitPublishCreatesBranchOnEmptyRemote(t *testing.T) {
	url := newBareRemote(t)
	g := testGitPublisher(t, url)
	now := time.Unix(1700007200, 0)

	tree := testBuiltTree("all.hoodi.example.org", 2, 7)
	if err := g.Publish(context.Background(), []builtTree{tree}, now); err != nil {
		t.Fatal(err)
	}

	nodes, ok := remoteFile(t, url, "all.hoodi.example.org/nodes.json")
	if !ok {
		t.Fatal("nodes.json missing from the remote")
	}
	want, err := renderNodesJSON(tree)
	if err != nil {
		t.Fatal(err)
	}
	if nodes != string(want) {
		t.Errorf("remote nodes.json does not match the rendered bytes:\n%s", nodes)
	}
	info, ok := remoteFile(t, url, "all.hoodi.example.org/enrtree-info.json")
	if !ok {
		t.Fatal("enrtree-info.json missing from the remote")
	}
	for _, needle := range []string{`"seq": 7`, `"signature": "c2ln"`, `"links": []`} {
		if !strings.Contains(info, needle) {
			t.Errorf("enrtree-info.json is missing %s:\n%s", needle, info)
		}
	}
}

func TestGitPublishLeavesOtherTreeDirectoriesUntouched(t *testing.T) {
	url := newBareRemote(t)
	g := testGitPublisher(t, url)
	now := time.Unix(1700007200, 0)

	first := testBuiltTree("all.hoodi.example.org", 2, 7)
	if err := g.Publish(context.Background(), []builtTree{first}, now); err != nil {
		t.Fatal(err)
	}
	second := testBuiltTree("snap.hoodi.example.org", 1, 8)
	if err := g.Publish(context.Background(), []builtTree{second}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, ok := remoteFile(t, url, "all.hoodi.example.org/nodes.json"); !ok {
		t.Error("a cycle without the all-tree deleted its directory")
	}
	if _, ok := remoteFile(t, url, "snap.hoodi.example.org/nodes.json"); !ok {
		t.Error("the new tree directory was not written")
	}
	if n := remoteCommits(t, url); n != 2 {
		t.Errorf("remote has %d commits, want 2", n)
	}
}

func TestGitPublishSkipsCommitWhenNothingChanged(t *testing.T) {
	url := newBareRemote(t)
	g := testGitPublisher(t, url)
	now := time.Unix(1700007200, 0)
	tree := testBuiltTree("all.hoodi.example.org", 2, 7)

	for i := 0; i < 2; i++ {
		if err := g.Publish(context.Background(), []builtTree{tree}, now); err != nil {
			t.Fatal(err)
		}
	}
	if n := remoteCommits(t, url); n != 1 {
		t.Errorf("remote has %d commits after an identical re-publish, want 1", n)
	}
}

func TestGitPublishRecoversFromRemoteRewrite(t *testing.T) {
	url := newBareRemote(t)
	g := testGitPublisher(t, url)
	now := time.Unix(1700007200, 0)
	if err := g.Publish(context.Background(), []builtTree{testBuiltTree("all.hoodi.example.org", 2, 7)}, now); err != nil {
		t.Fatal(err)
	}
	forcePushForeignHistory(t, url)

	if err := g.Publish(context.Background(), []builtTree{testBuiltTree("all.hoodi.example.org", 3, 9)}, now.Add(time.Hour)); err != nil {
		t.Fatalf("publish did not recover from a rewritten remote: %v", err)
	}
	if _, ok := remoteFile(t, url, "foreign.txt"); !ok {
		t.Error("the rewritten remote's own file was lost")
	}
	if _, ok := remoteFile(t, url, "all.hoodi.example.org/nodes.json"); !ok {
		t.Error("the tree was not published on top of the rewritten remote")
	}
}

// forcePushForeignHistory replaces the remote branch with unrelated history, the way a manual
// force-push would.
func forcePushForeignHistory(t *testing.T, url string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("master")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "foreign.txt"), []byte("rewritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("foreign.txt"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "foreign", Email: "foreign@test.invalid", When: time.Unix(1700000000, 0)}
	if _, err := wt.Commit("rewrite", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	err = repo.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/heads/master:refs/heads/master")},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGitPublishRefusesSymlinkedTreePath(t *testing.T) {
	url := newBareRemote(t)
	seedRemoteSymlink(t, url, "all.hoodi.example.org")
	g := testGitPublisher(t, url)

	err := g.Publish(context.Background(), []builtTree{testBuiltTree("all.hoodi.example.org", 1, 7)}, time.Unix(1700007200, 0))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("published through a symlinked tree path: %v", err)
	}
}

func seedRemoteSymlink(t *testing.T, url, name string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("master")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(name); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "seed", Email: "seed@test.invalid", When: time.Unix(1700000000, 0)}
	if _, err := wt.Commit("seed symlink", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatal(err)
	}
}

func TestRenderNodesJSONGolden(t *testing.T) {
	tree := testBuiltTree("all.hoodi.example.org", 2, 7)
	got, err := renderNodesJSON(tree)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
    "` + fmt.Sprintf("%064d", 0) + `": {
        "seq": 1,
        "record": "enr:-node0",
        "score": 10,
        "firstResponse": "2023-11-14T22:13:20Z",
        "lastResponse": "2023-11-14T23:13:20Z",
        "lastCheck": "2023-11-14T23:13:20Z"
    },
    "` + fmt.Sprintf("%064d", 1) + `": {
        "seq": 2,
        "record": "enr:-node1",
        "score": 20,
        "firstResponse": "2023-11-14T22:13:20Z",
        "lastResponse": "2023-11-14T23:13:20Z",
        "lastCheck": "2023-11-14T23:13:20Z"
    }
}
`
	if string(got) != want {
		t.Errorf("nodes.json bytes differ from the devp2p nodeset format:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderTreeInfoGolden(t *testing.T) {
	tree := testBuiltTree("all.hoodi.example.org", 1, 7)
	got, err := renderTreeInfo(tree, time.Unix(1700007200, 123456789).UTC())
	if err != nil {
		t.Fatal(err)
	}
	want := `{
    "url": "enrtree://KEY@all.hoodi.example.org",
    "seq": 7,
    "signature": "c2ln",
    "links": [],
    "lastModified": "2023-11-15T00:13:20.123456789Z"
}
`
	if string(got) != want {
		t.Errorf("enrtree-info.json bytes differ:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestNewGitPublisherRejectsWorldReadableKey(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, testSSHKeyPEM(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newGitPublisher("git@example.org:o/r.git", "master", dir, keyFile, keyFile, "n", "e"); err == nil {
		t.Fatal("accepted a world-readable deploy key")
	}
}

func TestNewGitPublisherRequiresKnownHosts(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, testSSHKeyPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newGitPublisher("git@example.org:o/r.git", "master", dir, keyFile, filepath.Join(dir, "missing"), "n", "e"); err == nil {
		t.Fatal("accepted a missing known_hosts file")
	}
}

func testSSHKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := xssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}
