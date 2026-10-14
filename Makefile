# Everything runs inside the dev image (Go, strace, SQLite) unless you call
# the targets with LOCAL=1 on a Linux machine that has them.
IMAGE  := wal-crash-dev
ifeq ($(LOCAL),1)
RUN    :=
else
RUN    := docker run --rm -u $$(id -u):$$(id -g) -e HOME=/tmp -e GOCACHE=/w/.cache/go \
          -e GOMODCACHE=/w/.cache/mod -v $(CURDIR):/w -w /w $(IMAGE)
endif

.PHONY: image build test matrix check demo clean

image:
ifneq ($(LOCAL),1)
	docker build -q -t $(IMAGE) docker/ >/dev/null
endif

build: image
	$(RUN) sh -c 'go build -o bin/ ./cmd/wal-crash ./examples/kvstore && \
	  gcc -O2 -Wall -o bin/sqlite-workload examples/sqlite/workload.c -lsqlite3'

test: image
	$(RUN) sh -c 'test -z "$$(gofmt -l .)" && go vet ./... && go test -race ./...'

# The README's tables, recomputed. Fails if a verdict changed.
matrix: build
	$(RUN) go run ./examples/matrix -bin bin -check | tee docs/matrix.txt

check: test matrix

demo: build
	docker run --rm -v $(CURDIR):/vhs -w /vhs --entrypoint sh ghcr.io/charmbracelet/vhs:v0.10.0 \
	  -c 'sed -i "s/stable/bookworm/g" /etc/apt/sources.list.d/debian.sources && apt-get update -qq >/dev/null && apt-get install -y -qq strace sqlite3 libsqlite3-dev gcc >/dev/null && \
	      gcc -O2 -o bin/sqlite-workload examples/sqlite/workload.c -lsqlite3 && vhs docs/demo.tape; s=$$?; chown -R $$(stat -c %u:%g Makefile) docs bin; exit $$s'
	$(MAKE) build

clean:
	rm -rf bin .cache .out
