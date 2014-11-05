# vcsstore

vcsstore stores VCS repositories and makes them accessible via HTTP.

[![Build Status](https://travis-ci.org/sourcegraph/vcsstore.png?branch=master)](https://travis-ci.org/sourcegraph/vcsstore)

## Install

* Install libssh2 and libssl-dev: `apt-get install libssh2-1-dev libssl-dev`
* Run `make build-libgit2`
* `godep go install ./cmd/vcsstore`
* `vcsstore serve`

The included Dockerfile exposes vcsstore on container port 80. To
expose it on host port 9090 and have it store VCS data in
/tmp/vcsstore on the host, run:

```
docker build -t vcsstore .
docker run -p 9090:80 -v /tmp/vcsstore vcsstore
```

vcsstore (and vcsclient in particular) can also be used as a library.

## Related reading

* [How We Made GitHub Fast (GitHub blog post)](https://github.com/blog/530-how-we-made-github-fast)

## Authors

* Quinn Slack <sqs@sourcegraph.com>
