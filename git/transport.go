package git

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"log"
	"os"
	"os/exec"

	"github.com/AaronO/go-git-http"
)

// GitTransport represents a git repository with all the functions to
// support the "smart" transfer protocol.
type GitTransport interface {
	// InfoRefs writes the output of git-info-refs to w. If this
	// function returns an error, then nothing is written to w.
	InfoRefs(w io.Writer, service string) error

	// ReceivePack writes the output of git-receive-pack to w, reading
	// from rc. If this function returns an error, then nothing is
	// written to w.
	ReceivePack(w io.Writer, rc io.ReadCloser, opt GitTransportOpt) error

	// UploadPack writes the output of git-upload-pack to w, reading
	// from rc. If this function returns an error, then nothing is
	// written to w.
	UploadPack(w io.Writer, rc io.ReadCloser, opt GitTransportOpt) error
}

type GitTransportOpt struct {
	ContentEncoding string
}

func NewLocalGitTransport(dir string) GitTransport {
	return &localGitTransport{dir: dir}
}

// localGitTransport is a git repository hosted on local disk
type localGitTransport struct {
	dir string
}

// TODO(security): should we validate 'service'?
func (r *localGitTransport) InfoRefs(w io.Writer, service string) error {
	cmd := exec.Command("git", service, "--stateless-rpc", "--advertise-refs", ".")
	cmd.Dir = r.dir
	cmd.Stdout, cmd.Stderr = w, os.Stderr
	return cmd.Run()
}

func (r *localGitTransport) ReceivePack(w io.Writer, rc io.ReadCloser, opt GitTransportOpt) error {
	return r.servicePack("receive-pack", w, rc, opt)
}

func (r *localGitTransport) UploadPack(w io.Writer, rc io.ReadCloser, opt GitTransportOpt) error {
	return r.servicePack("upload-pack", w, rc, opt)
}

func (r *localGitTransport) servicePack(service string, w io.Writer, rc io.ReadCloser, opt GitTransportOpt) error {
	var err error
	switch opt.ContentEncoding {
	case "gzip":
		rc, err = gzip.NewReader(rc)
	case "deflate":
		rc = flate.NewReader(rc)
	}
	if err != nil {
		return err
	}

	rpcReader := &githttp.RpcReader{
		ReadCloser: rc,
		Rpc:        service,
	}

	cmd := exec.Command("git", service, "--stateless-rpc", ".")
	cmd.Dir = r.dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer stdout.Close()

	err = cmd.Start()
	if err != nil {
		return err
	}

	// Scan's git command's output for errors
	gitReader := &githttp.GitReader{
		ReadCloser: stdout,
	}

	// Copy input to git binary
	io.Copy(stdin, rpcReader)

	// Write git binary's output to http response
	io.Copy(w, gitReader)

	// Wait till command has completed
	mainError := cmd.Wait()
	if mainError == nil {
		mainError = gitReader.GitError
	}
	for _, e := range rpcReader.Events {
		log.Printf("EVENT: %q\n", e)
	}
	return mainError
}
