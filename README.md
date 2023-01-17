# kubectl-pdebug

[![Build Status](https://github.com/terrycain/kubectl-pdebug/actions/workflows/build.yaml/badge.svg)](https://github.com/terrycain/kubectl-pdebug/actions)
[![Go Report Card](https://goreportcard.com/badge/terrycain/kubectl-pdebug)](https://goreportcard.com/report/terrycain/kubectl-pdebug)
[![LICENSE](https://img.shields.io/github/license/terrycain/kubectl-pdebug.svg)](https://github.com/terrycain/kubectl-pdebug/blob/master/LICENSE)
[![Releases](https://img.shields.io/github/release-pre/terrycain/kubectl-pdebug.svg)](https://github.com/terrycain/kubectl-pdebug/releases)

Run privileged ephemeral debug containers.

## Intro

This is a near clone of `kubectl debug` focusing on running ephemeral containers alongside pods whilst
allowing you to specify additional capabilities or to run as privileged.

Main reasoning for this was to run strace with the `SYS_PTRACE` capability

## Example

```shell
# kubectl pdebug -n somenamespace pod-sbsv5 --cap-add=SYS_PTRACE -it --image=nicolaka/netshoot --target app -- sh
> strace ...
```

Run `kubectl pdebug --help` for the options.

## Installation

### Manually

Download the binary from the GitHub releases page [here](https://github.com/terrycain/kubectl-pdebug/releases).
Rename to match `kubectl-pdebug` and move into `$PATH`.

```shell
curl -o /tmp/kubectl-pdebug -L https://github.com/terrycain/kubectl-pdebug/releases/download/v0.1.1/kubectl-pdebug_0.1.1_linux_amd64
sudo install --group=root --owner=root --mode=0755 /tmp/kubectl-pdebug /usr/local/bin/kubectl-pdebug
rm /tmp/kubectl-pdebug
```

### Via Krew

Coming soon.