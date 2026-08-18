package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

const (
	maxGitSSHKeyBytes = 16 << 10
	gitPublishTimeout = 5 * time.Minute
)

// gitPublisher pushes each published tree's node list to a git repo, continuing the
// ethereum/discv4-dns-lists contract the legacy discv4-crawl served. Unlike the legacy crawler,
// git is output-only here: crawl state lives in S3 snapshots and the sequence floor in the
// .published artifacts, so a failed push costs a commit, never correctness.
type gitPublisher struct {
	repoURL string
	branch  string
	dir     string
	auth    transport.AuthMethod
	name    string
	email   string
	// depth 1 in production; 0 (full) in tests, whose in-process pack server cannot serve shallow.
	depth   int
	timeout time.Duration
}

func newGitPublisher(repoURL, branch, dir, keyFile, knownHostsFile, name, email string) (*gitPublisher, error) {
	key, err := readPrivateFile(keyFile, "git ssh key", maxGitSSHKeyBytes)
	if err != nil {
		return nil, err
	}
	ep, err := transport.NewEndpoint(repoURL)
	if err != nil {
		return nil, fmt.Errorf("parse --git-repo-url: %w", err)
	}
	user := ep.User
	if user == "" {
		user = "git"
	}
	auth, err := gitssh.NewPublicKeys(user, key, "")
	if err != nil {
		return nil, fmt.Errorf("parse git ssh key: %w", err)
	}
	// The callback needs an explicit known_hosts path: the container has no $HOME, and host keys
	// must be pinned rather than trusted on first use.
	callback, err := gitssh.NewKnownHostsCallback(knownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("load git known hosts: %w", err)
	}
	auth.HostKeyCallback = callback
	return &gitPublisher{
		repoURL: repoURL, branch: branch, dir: dir, auth: auth,
		name: name, email: email, depth: 1, timeout: gitPublishTimeout,
	}, nil
}

// Publish clones fresh at depth 1 every cycle, writes each tree's directory, and pushes one
// commit. The re-clone is deliberate: go-git's fetch-into-shallow path is its buggy area, and
// starting from the remote's truth means a rewritten remote or corrupt local state can never
// wedge pushes the way the legacy crawler's clone-once loop could. Directories for trees not in
// this cycle keep the remote's content; nothing is ever deleted.
func (g *gitPublisher) Publish(ctx context.Context, trees []builtTree, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	checkout := filepath.Join(g.dir, "checkout")
	if err := os.RemoveAll(checkout); err != nil {
		return fmt.Errorf("clear checkout: %w", err)
	}
	repo, err := g.clone(ctx, checkout)
	if err != nil {
		return err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	summary := make([]string, 0, len(trees))
	for _, t := range trees {
		dir := filepath.Join(checkout, t.Domain)
		if err := refuseSymlink(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		nodes, err := renderNodesJSON(t)
		if err != nil {
			return err
		}
		info, err := renderTreeInfo(t, now)
		if err != nil {
			return err
		}
		for name, content := range map[string][]byte{"nodes.json": nodes, "enrtree-info.json": info} {
			path := filepath.Join(dir, name)
			if err := refuseSymlink(path); err != nil {
				return err
			}
			if err := os.WriteFile(path, content, 0o644); err != nil {
				return err
			}
			if _, err := wt.Add(filepath.Join(t.Domain, name)); err != nil {
				return fmt.Errorf("stage %s/%s: %w", t.Domain, name, err)
			}
		}
		summary = append(summary, fmt.Sprintf("%s=%d", t.Domain, t.Nodes))
	}
	status, err := wt.Status()
	if err != nil {
		return err
	}
	if status.IsClean() {
		return nil
	}
	// Explicit identity: distroless has no .gitconfig, and go-git errors resolving a default.
	sig := &object.Signature{Name: g.name, Email: g.email, When: now.UTC()}
	msg := fmt.Sprintf("update %d trees: %s", len(trees), strings.Join(summary, " "))
	if _, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	refspec := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", g.branch, g.branch))
	err = repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin", RefSpecs: []config.RefSpec{refspec}, Auth: g.auth,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

func (g *gitPublisher) clone(ctx context.Context, checkout string) (*git.Repository, error) {
	repo, err := git.PlainCloneContext(ctx, checkout, false, &git.CloneOptions{
		URL:           g.repoURL,
		Auth:          g.auth,
		Depth:         g.depth,
		SingleBranch:  true,
		ReferenceName: plumbing.NewBranchReferenceName(g.branch),
	})
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil, fmt.Errorf("clone %s: %w", g.repoURL, err)
	}
	// A fresh remote has no branch to clone; the first push from an initialized checkout creates it.
	repo, err = git.PlainInitWithOptions(checkout, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(g.branch)},
	})
	if err != nil {
		return nil, fmt.Errorf("init empty checkout: %w", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{g.repoURL}}); err != nil {
		return nil, fmt.Errorf("add origin: %w", err)
	}
	return repo, nil
}

// The devp2p nodeset file format (the discv4-dns-lists contract), reimplemented from the on-disk
// data - never from go-ethereum's GPL cmd/devp2p code. lastResponse carries the last successful
// resolution, not LastSeen, which also advances on failed checks. firstResponse carries FirstSeen,
// the first observation: the schema keeps no first-success timestamp, and every published node has
// since been resolved and dialed, so the drift is bounded and early rather than wrong.
type nodeJSON struct {
	Seq           uint64 `json:"seq"`
	Record        string `json:"record"`
	Score         int32  `json:"score"`
	FirstResponse string `json:"firstResponse"`
	LastResponse  string `json:"lastResponse"`
	LastCheck     string `json:"lastCheck"`
}

func renderNodesJSON(t builtTree) ([]byte, error) {
	set := make(map[string]nodeJSON, len(t.nodes))
	for _, n := range t.nodes {
		set[n.id] = nodeJSON{
			Seq:           n.seq,
			Record:        n.record,
			Score:         n.score,
			FirstResponse: gitTime(n.firstSeen),
			LastResponse:  gitTime(n.lastResolved),
			LastCheck:     gitTime(n.lastCheck),
		}
	}
	out, err := json.MarshalIndent(set, "", "    ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

type treeInfoJSON struct {
	URL          string   `json:"url"`
	Seq          uint64   `json:"seq"`
	Signature    string   `json:"signature"`
	Links        []string `json:"links"`
	LastModified string   `json:"lastModified"`
}

func renderTreeInfo(t builtTree, now time.Time) ([]byte, error) {
	out, err := json.MarshalIndent(treeInfoJSON{
		URL:          t.URL,
		Seq:          t.Seq,
		Signature:    t.signature,
		Links:        []string{},
		LastModified: now.UTC().Format(time.RFC3339Nano),
	}, "", "    ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func gitTime(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// A tracked symlink in the cloned repo would send MkdirAll or WriteFile outside the checkout, so a
// remote commit could redirect the publisher's writes at anything its user can touch.
func refuseSymlink(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", path)
	}
	return nil
}
