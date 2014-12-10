package gitcmd

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"sourcegraph.com/sourcegraph/go-vcs/vcs"
	"sourcegraph.com/sourcegraph/go-vcs/vcs/util"

	"golang.org/x/tools/godoc/vfs"
)

func init() {
	vcs.RegisterOpener("git", func(dir string) (vcs.Repository, error) {
		return Open(dir)
	})
	vcs.RegisterCloner("git", func(url, dir string, opt vcs.CloneOpt) (vcs.Repository, error) {
		return Clone(url, dir, opt)
	})
}

type Repository struct {
	Dir string

	editLock sync.RWMutex // protects ops that change repository data
}

func Open(dir string) (*Repository, error) {
	return &Repository{Dir: dir}, nil
}

func Clone(url, dir string, opt vcs.CloneOpt) (*Repository, error) {
	args := []string{"clone"}
	if opt.Bare {
		args = append(args, "--bare")
	}
	if opt.Mirror {
		args = append(args, "--mirror")
	}
	args = append(args, "--", url, dir)
	cmd := exec.Command("git", args...)

	if opt.SSH != nil {
		gitSSHWrapper, keyFile, err := makeGitSSHWrapper(opt.SSH.PrivateKey)
		defer func() {
			if keyFile != "" {
				if err := os.Remove(keyFile); err != nil {
					log.Fatalf("Error removing SSH key file %s: %s.", keyFile, err)
				}
			}
		}()
		if err != nil {
			return nil, err
		}
		defer os.Remove(gitSSHWrapper)
		cmd.Env = []string{"GIT_SSH=" + gitSSHWrapper}
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("exec `git clone` failed: %s. Output was:\n\n%s", err, out)
	}
	return Open(dir)
}

// checkSpecArgSafety returns a non-nil err if spec begins with a "-", which could
// cause it to be interpreted as a git command line argument.
func checkSpecArgSafety(spec string) error {
	if strings.HasPrefix(spec, "-") {
		return errors.New("invalid git revision spec (begins with '-')")
	}
	return nil
}

func (r *Repository) ResolveRevision(spec string) (vcs.CommitID, error) {
	r.editLock.RLock()
	defer r.editLock.RUnlock()

	if err := checkSpecArgSafety(spec); err != nil {
		return "", err
	}

	cmd := exec.Command("git", "rev-parse", spec)
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if bytes.Contains(out, []byte("unknown revision")) {
			return "", vcs.ErrRevisionNotFound
		}
		return "", fmt.Errorf("exec `git rev-parse` failed: %s. Output was:\n\n%s", err, out)
	}
	return vcs.CommitID(bytes.TrimSpace(out)), nil
}

func (r *Repository) ResolveBranch(name string) (vcs.CommitID, error) {
	commitID, err := r.ResolveRevision(name)
	if err == vcs.ErrRevisionNotFound {
		return "", vcs.ErrBranchNotFound
	}
	return commitID, nil
}

func (r *Repository) ResolveTag(name string) (vcs.CommitID, error) {
	commitID, err := r.ResolveRevision(name)
	if err == vcs.ErrRevisionNotFound {
		return "", vcs.ErrTagNotFound
	}
	return commitID, nil
}

func (r *Repository) Branches() ([]*vcs.Branch, error) {
	r.editLock.RLock()
	defer r.editLock.RUnlock()

	refs, err := r.showRef("--heads")
	if err != nil {
		return nil, err
	}

	branches := make([]*vcs.Branch, len(refs))
	for i, ref := range refs {
		branches[i] = &vcs.Branch{
			Name: strings.TrimPrefix(ref[1], "refs/heads/"),
			Head: vcs.CommitID(ref[0]),
		}
	}
	return branches, nil
}

func (r *Repository) Tags() ([]*vcs.Tag, error) {
	r.editLock.RLock()
	defer r.editLock.RUnlock()

	refs, err := r.showRef("--tags")
	if err != nil {
		return nil, err
	}

	tags := make([]*vcs.Tag, len(refs))
	for i, ref := range refs {
		tags[i] = &vcs.Tag{
			Name:     strings.TrimPrefix(ref[1], "refs/tags/"),
			CommitID: vcs.CommitID(ref[0]),
		}
	}
	return tags, nil
}

type byteSlices [][]byte

func (p byteSlices) Len() int           { return len(p) }
func (p byteSlices) Less(i, j int) bool { return bytes.Compare(p[i], p[j]) < 0 }
func (p byteSlices) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

func (r *Repository) showRef(arg string) ([][2]string, error) {
	cmd := exec.Command("git", "show-ref", arg)
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Exit status of 1 and no output means there were no
		// results. This is not a fatal error.
		if exitStatus(err) == 1 && len(out) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("exec `git show-ref %s` in %s failed: %s. Output was:\n\n%s", arg, r.Dir, err, out)
	}

	out = bytes.TrimSuffix(out, []byte("\n")) // remove trailing newline
	lines := bytes.Split(out, []byte("\n"))
	sort.Sort(byteSlices(lines)) // sort for consistency
	refs := make([][2]string, len(lines))
	for i, line := range lines {
		if len(line) <= 41 {
			return nil, errors.New("unexpectedly short (<=41 bytes) line in `git show-ref ...` output")
		}
		id := line[:40]
		name := line[41:]
		refs[i] = [2]string{string(id), string(name)}
	}
	return refs, nil
}

func exitStatus(err error) uint32 {
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			// There is no platform independent way to retrieve
			// the exit code, but the following will work on Unix
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				return uint32(status.ExitStatus())
			}
		}
		return 0
	}
	return 0
}

func (r *Repository) GetCommit(id vcs.CommitID) (*vcs.Commit, error) {
	r.editLock.RLock()
	defer r.editLock.RUnlock()

	if err := checkSpecArgSafety(string(id)); err != nil {
		return nil, err
	}

	commits, _, err := r.commitLog(vcs.CommitsOptions{Head: id, N: 1})
	if err != nil {
		return nil, err
	}

	if len(commits) != 1 {
		return nil, fmt.Errorf("git log: expected 1 commit, got %d", len(commits))
	}

	return commits[0], nil
}

func (r *Repository) Commits(opt vcs.CommitsOptions) ([]*vcs.Commit, uint, error) {
	r.editLock.RLock()
	defer r.editLock.RUnlock()

	if err := checkSpecArgSafety(string(opt.Head)); err != nil {
		return nil, 0, err
	}

	return r.commitLog(opt)
}

func isBadObjectErr(output, obj string) bool {
	return string(output) == "fatal: bad object "+obj
}

func (r *Repository) commitLog(opt vcs.CommitsOptions) ([]*vcs.Commit, uint, error) {
	args := []string{"log", `--format=format:%H%x00%aN%x00%aE%x00%at%x00%cN%x00%cE%x00%ct%x00%B%x00%P%x00`}
	if opt.N != 0 {
		args = append(args, "-n", strconv.FormatUint(uint64(opt.N), 10))
	}
	if opt.Skip != 0 {
		args = append(args, "--skip="+strconv.FormatUint(uint64(opt.Skip), 10))
	}
	args = append(args, string(opt.Head))

	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		out = bytes.TrimSpace(out)
		if isBadObjectErr(string(out), string(opt.Head)) {
			return nil, 0, vcs.ErrCommitNotFound
		}
		return nil, 0, fmt.Errorf("exec `git log` failed: %s. Output was:\n\n%s", err, out)
	}

	const partsPerCommit = 9 // number of \x00-separated fields per commit
	allParts := bytes.Split(out, []byte{'\x00'})
	numCommits := len(allParts) / partsPerCommit
	commits := make([]*vcs.Commit, numCommits)
	for i := 0; i < numCommits; i++ {
		parts := allParts[partsPerCommit*i : partsPerCommit*(i+1)]

		// log outputs are newline separated, so all but the 1st commit ID part
		// has an erroneous leading newline.
		parts[0] = bytes.TrimPrefix(parts[0], []byte{'\n'})

		authorTime, err := strconv.ParseInt(string(parts[3]), 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("parsing git commit author time: %s", err)
		}
		committerTime, err := strconv.ParseInt(string(parts[6]), 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("parsing git commit committer time: %s", err)
		}

		var parents []vcs.CommitID
		if parentPart := parts[8]; len(parentPart) > 0 {
			parentIDs := bytes.Split(parentPart, []byte{' '})
			parents = make([]vcs.CommitID, len(parentIDs))
			for i, id := range parentIDs {
				parents[i] = vcs.CommitID(id)
			}
		}

		commits[i] = &vcs.Commit{
			ID:        vcs.CommitID(parts[0]),
			Author:    vcs.Signature{string(parts[1]), string(parts[2]), time.Unix(authorTime, 0)},
			Committer: &vcs.Signature{string(parts[4]), string(parts[5]), time.Unix(committerTime, 0)},
			Message:   string(bytes.TrimSuffix(parts[7], []byte{'\n'})),
			Parents:   parents,
		}
	}

	// Count commits.
	cmd = exec.Command("git", "rev-list", "--count", string(opt.Head))
	cmd.Dir = r.Dir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return nil, 0, fmt.Errorf("exec `git rev-list --count` failed: %s. Output was:\n\n%s", err, out)
	}
	out = bytes.TrimSpace(out)
	total, err := strconv.ParseUint(string(out), 10, 64)
	if err != nil {
		return nil, 0, err
	}

	return commits, uint(total), nil
}

func (r *Repository) Diff(base, head vcs.CommitID, opt *vcs.DiffOptions) (*vcs.Diff, error) {
	r.editLock.RLock()
	defer r.editLock.RUnlock()

	if strings.HasPrefix(string(base), "-") || strings.HasPrefix(string(head), "-") {
		// Protect against base or head that is interpreted as command-line option.
		return nil, errors.New("diff revspecs must not start with '-'")
	}

	if opt == nil {
		opt = &vcs.DiffOptions{}
	}
	args := []string{"diff", "--full-index"}
	if opt.DetectRenames {
		args = append(args, "-M")
	}
	args = append(args, "--src-prefix="+opt.OrigPrefix)
	args = append(args, "--dst-prefix="+opt.NewPrefix)
	args = append(args, string(base), string(head), "--")
	cmd := exec.Command("git", args...)
	if opt != nil {
		cmd.Args = append(cmd.Args, opt.Paths...)
	}
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		out = bytes.TrimSpace(out)
		if isBadObjectErr(string(out), string(base)) || isBadObjectErr(string(out), string(head)) {
			return nil, vcs.ErrCommitNotFound
		}
		return nil, fmt.Errorf("exec `git diff` failed: %s. Output was:\n\n%s", err, out)
	}
	return &vcs.Diff{
		Raw: string(out),
	}, nil
}

// A CrossRepoDiffHead is a git repository that can be used as the
// head repository for a cross-repo diff (in another git repository's
// CrossRepoDiff method).
type CrossRepoDiffHead interface {
	GitRootDir() string // the repo's root directory
}

func (r *Repository) GitRootDir() string { return r.Dir }

func (r *Repository) CrossRepoDiff(base vcs.CommitID, headRepo vcs.Repository, head vcs.CommitID, opt *vcs.DiffOptions) (*vcs.Diff, error) {
	var headDir string // path to head repo on local filesystem
	if headRepo, ok := headRepo.(CrossRepoDiffHead); ok {
		headDir = headRepo.GitRootDir()
	} else {
		return nil, fmt.Errorf("git cross-repo diff not supported against head repo type %T", headRepo)
	}

	if headDir == r.Dir {
		return r.Diff(base, head, opt)
	}

	fetch := func() error {
		// Wrap in goroutine so we can use defer to release lock.
		r.editLock.Lock()
		defer r.editLock.Unlock()

		// Fetch remote commit data.
		cmd := exec.Command("git", "fetch", headDir)
		cmd.Dir = r.Dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("exec %v in %s failed: %s. Output was:\n\n%s", cmd.Args, cmd.Dir, err, out)
		}
		return nil
	}
	if err := fetch(); err != nil {
		return nil, err
	}

	return r.Diff(base, head, opt)
}

func (r *Repository) UpdateEverything(opt vcs.RemoteOpts) error {
	// TODO(sqs): this lock is different from libgit2's lock, but
	// libgit2 Repositories call this method because of
	// embedding. Therefore there could be a race condition.
	r.editLock.Lock()
	defer r.editLock.Unlock()

	cmd := exec.Command("git", "remote", "update")
	cmd.Dir = r.Dir

	if opt.SSH != nil {
		if opt.SSH != nil {
			gitSSHWrapper, keyFile, err := makeGitSSHWrapper(opt.SSH.PrivateKey)
			defer func() {
				if keyFile != "" {
					if err := os.Remove(keyFile); err != nil {
						log.Fatalf("Error removing SSH key file %s: %s.", keyFile, err)
					}
				}
			}()
			if err != nil {
				return err
			}
			defer os.Remove(gitSSHWrapper)
			cmd.Env = []string{"GIT_SSH=" + gitSSHWrapper}
		}
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exec `git remote update` failed: %s. Output was:\n\n%s", err, out)
	}
	return nil
}

func (r *Repository) BlameFile(path string, opt *vcs.BlameOptions) ([]*vcs.Hunk, error) {
	r.editLock.RLock()
	defer r.editLock.RUnlock()

	if opt == nil {
		opt = &vcs.BlameOptions{}
	}
	if opt.OldestCommit != "" {
		return nil, fmt.Errorf("OldestCommit not implemented")
	}
	if err := checkSpecArgSafety(string(opt.NewestCommit)); err != nil {
		return nil, err
	}
	if err := checkSpecArgSafety(string(opt.OldestCommit)); err != nil {
		return nil, err
	}

	cmd := exec.Command("git", "blame", "-w", "--porcelain", string(opt.NewestCommit), "--", path)
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("exec `git blame` failed: %s. Output was:\n\n%s", err, out)
	}
	if len(out) < 1 {
		// go 1.8.5 changed the behavior of `git blame` on empty files.
		// previously, it returned a boundary commit. now, it returns nothing.
		// TODO(sqs) TODO(beyang): make `git blame` return the boundary commit
		// on an empty file somehow, or come up with some other workaround.
		st, err := os.Stat(filepath.Join(r.Dir, path))
		if err == nil && st.Size() == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("Expected git output of length at least 1")
	}

	commits := make(map[string]vcs.Commit)
	hunks := make([]*vcs.Hunk, 0)
	remainingLines := strings.Split(string(out[:len(out)-1]), "\n")
	byteOffset := 0
	for len(remainingLines) > 0 {
		// Consume hunk
		hunkHeader := strings.Split(remainingLines[0], " ")
		if len(hunkHeader) != 4 {
			fmt.Printf("Remaining lines: %+v, %d, '%s'\n", remainingLines, len(remainingLines), remainingLines[0])
			return nil, fmt.Errorf("Expected at least 4 parts to hunkHeader, but got: '%s'", hunkHeader)
		}
		commitID := hunkHeader[0]
		lineNoCur, _ := strconv.Atoi(hunkHeader[2])
		nLines, _ := strconv.Atoi(hunkHeader[3])
		hunk := &vcs.Hunk{
			CommitID:  vcs.CommitID(commitID),
			StartLine: int(lineNoCur),
			EndLine:   int(lineNoCur + nLines),
			StartByte: byteOffset,
		}

		if _, in := commits[commitID]; in {
			// Already seen commit
			byteOffset += len(remainingLines[1])
			remainingLines = remainingLines[2:]
		} else {
			// New commit
			author := strings.Join(strings.Split(remainingLines[1], " ")[1:], " ")
			email := strings.Join(strings.Split(remainingLines[2], " ")[1:], " ")
			if len(email) >= 2 && email[0] == '<' && email[len(email)-1] == '>' {
				email = email[1 : len(email)-1]
			}
			authorTime, err := strconv.ParseInt(strings.Join(strings.Split(remainingLines[3], " ")[1:], " "), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("Failed to parse author-time %q", remainingLines[3])
			}
			summary := strings.Join(strings.Split(remainingLines[9], " ")[1:], " ")
			commit := vcs.Commit{
				ID:      vcs.CommitID(commitID),
				Message: summary,
				Author: vcs.Signature{
					Name:  author,
					Email: email,
					Date:  time.Unix(authorTime, 0).In(time.UTC),
				},
			}
			hunk.CommitID = commit.ID
			hunk.Author = commit.Author

			if len(remainingLines) >= 13 && strings.HasPrefix(remainingLines[10], "previous ") {
				byteOffset += len(remainingLines[12])
				remainingLines = remainingLines[13:]
			} else if len(remainingLines) >= 13 && remainingLines[10] == "boundary" {
				byteOffset += len(remainingLines[12])
				remainingLines = remainingLines[13:]
			} else if len(remainingLines) >= 12 {
				byteOffset += len(remainingLines[11])
				remainingLines = remainingLines[12:]
			} else if len(remainingLines) == 11 {
				// Empty file
				remainingLines = remainingLines[11:]
			} else {
				return nil, fmt.Errorf("Unexpected number of remaining lines (%d):\n%s", len(remainingLines), "  "+strings.Join(remainingLines, "\n  "))
			}

			commits[commitID] = commit
		}

		// Consume remaining lines in hunk
		for i := 1; i < nLines; i++ {
			byteOffset += len(remainingLines[1])
			remainingLines = remainingLines[2:]
		}

		hunk.EndByte = byteOffset
		hunks = append(hunks, hunk)
	}

	return hunks, nil
}

func (r *Repository) FileSystem(at vcs.CommitID) (vfs.FileSystem, error) {
	if err := checkSpecArgSafety(string(at)); err != nil {
		return nil, err
	}

	return &gitFSCmd{
		dir:          r.Dir,
		at:           at,
		repoEditLock: &r.editLock,
	}, nil
}

type gitFSCmd struct {
	dir          string
	at           vcs.CommitID
	repoEditLock *sync.RWMutex
}

func (fs *gitFSCmd) Open(name string) (vfs.ReadSeekCloser, error) {
	fs.repoEditLock.RLock()
	defer fs.repoEditLock.RUnlock()

	cmd := exec.Command("git", "show", string(fs.at)+":"+name)
	cmd.Dir = fs.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if bytes.Contains(out, []byte("exists on disk, but not in")) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("exec `git show` failed: %s. Output was:\n\n%s", err, out)
	}
	return util.NopCloser{bytes.NewReader(out)}, nil
}

func (fs *gitFSCmd) Lstat(path string) (os.FileInfo, error) {
	fs.repoEditLock.RLock()
	defer fs.repoEditLock.RUnlock()

	return fs.Stat(path)
}

func (fs *gitFSCmd) Stat(path string) (os.FileInfo, error) {
	fs.repoEditLock.RLock()
	defer fs.repoEditLock.RUnlock()

	path = filepath.Clean(path)

	cmd := exec.Command("git", "log", "-1", "--format=%ad", string(fs.at),
		"--", path)
	cmd.Dir = fs.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}

	timeStr := strings.Trim(string(out), "\n")
	if timeStr == "" {
		return nil, os.ErrNotExist
	}

	mtime, err := time.Parse("Mon Jan _2 15:04:05 2006 -0700", timeStr)
	if err != nil {
		return nil, err
	}

	if path == "." {
		return &util.FileInfo{Mode_: os.ModeDir, ModTime_: mtime}, nil
	}

	// TODO(sqs): follow symlinks (as Stat is required to do)

	f, err := fs.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(data, []byte(fmt.Sprintf("tree %s:%s\n", fs.at, path))) {
		// dir
		return &util.FileInfo{Name_: filepath.Base(path), Mode_: os.ModeDir,
			ModTime_: mtime}, nil
	}

	return &util.FileInfo{Name_: filepath.Base(path), Size_: int64(len(data)),
		ModTime_: mtime}, nil
}

func (fs *gitFSCmd) ReadDir(path string) ([]os.FileInfo, error) {
	fs.repoEditLock.RLock()
	defer fs.repoEditLock.RUnlock()

	path = filepath.Clean(path)
	if err := checkSpecArgSafety(path); err != nil {
		return nil, err
	}

	cmd := exec.Command("git", "ls-tree", "-z", string(fs.at), path+"/")
	cmd.Dir = fs.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if bytes.Contains(out, []byte("exists on disk, but not in")) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("exec `git ls-files` failed: %s. Output was:\n\n%s", err, out)
	}

	// in `git show` output for dir, first line is header, 2nd line is blank,
	// and there is a trailing newline.
	lines := bytes.Split(out, []byte{'\x00'})
	fis := make([]os.FileInfo, len(lines)-1)
	for i, line := range lines {
		if i == len(lines)-1 {
			// last entry is empty
			continue
		}

		typ, name := string(line[7:11]), line[53:]

		var mode os.FileMode
		if typ == "tree" {
			mode = os.ModeDir
		} else if typ == "link" {
			mode = os.ModeSymlink
		}

		relName, err := filepath.Rel(path, string(name))
		if err != nil {
			return nil, err
		}
		fis[i] = &util.FileInfo{Name_: relName, Mode_: mode}
	}

	return fis, nil
}

func (fs *gitFSCmd) String() string {
	return fmt.Sprintf("git repository %s commit %s (cmd)", fs.dir, fs.at)
}

// makeGitSSHWrapper writes a GIT_SSH wrapper that runs ssh with the
// private key. You should close and remove the sshWrapper and remove
// the keyFile after using them.
func makeGitSSHWrapper(privKey []byte) (sshWrapper, keyFile string, err error) {
	var otherOpt string
	if InsecureSkipCheckVerifySSH {
		otherOpt = "-o StrictHostKeyChecking=no"
	}

	kf, err := ioutil.TempFile("", "go-vcs-gitcmd-key")
	if err != nil {
		return "", "", err
	}
	keyFile = kf.Name()
	if err := kf.Chmod(0600); err != nil {
		return "", keyFile, err
	}
	if _, err := kf.Write(privKey); err != nil {
		return "", keyFile, err
	}
	if err := kf.Close(); err != nil {
		return "", keyFile, err
	}

	// TODO(sqs): encrypt and store the key in the env so that
	// attackers can't decrypt if they have disk access after our
	// process dies
	script := `
	#!/bin/sh
	exec /usr/bin/ssh -o ControlMaster=no -o ControlPath=none ` + otherOpt + ` -i ` + keyFile + ` "$@"
`

	tf, err := ioutil.TempFile("", "go-vcs-gitcmd")
	if err != nil {
		return "", keyFile, err
	}
	tmpFile := tf.Name()
	if _, err := tf.WriteString(script); err != nil {
		return "", keyFile, err
	}
	if err := tf.Chmod(0500); err != nil {
		return "", "", err
	}
	if err := tf.Close(); err != nil {
		return "", "", err
	}

	return tmpFile, keyFile, nil
}

// InsecureSkipCheckVerifySSH controls whether the client verifies the
// SSH server's certificate or host key. If InsecureSkipCheckVerifySSH
// is true, the program is susceptible to a man-in-the-middle
// attack. This should only be used for testing.
var InsecureSkipCheckVerifySSH bool
