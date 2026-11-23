# go-build-tool OUT,PACKAGE builds a tool and publishes it at OUT only when
# its bytes changed, so an unchanged tool keeps its time. `go build -o`
# rewrites its output on every run, and Go keys a cached test result by the
# times of the entries of every directory a test lists, so a test that
# lists bin/ would rerun after each `make go-test` otherwise. Without VCS
# stamping the bytes change only with the tool's code, and the build runs no
# `git status` (the Makefile's go-test GOFLAGS say why that matters).
#
# The build goes to a directory of its own outside OUT's. A changed tool is
# copied to a staging file that mktemp names for this publisher alone, in
# OUT's directory so the rename stays on one filesystem, and renamed over
# OUT once the copy has closed it: another make publishing the same tool at
# the same time renames a whole file of its own, never this one's, and a
# reader sees the old tool or a whole new one. The EXIT trap removes the
# build directory and a staging file a failure left.
go-build-tool = build=$$(mktemp -d) && stage= && trap 'rm -rf "$$build"; [ -z "$$stage" ] || rm -f "$$stage"' EXIT && \
	go build -buildvcs=false -o "$$build/tool" $(2) && \
	{ cmp -s "$$build/tool" $(1) || { mkdir -p $(dir $(1)) && stage=$$(mktemp $(dir $(1)).$(notdir $(1)).XXXXXX) && \
	cp "$$build/tool" "$$stage" && chmod 0755 "$$stage" && mv -f "$$stage" $(1) && stage=; }; }
