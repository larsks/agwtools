prefix=/usr
bindir=$(prefix)/bin

GO=go
INSTALL=install

GOARCH=$(shell go env GOARCH)
GOOS=$(shell go env GOOS)

export GOARCH
export GOOS

AGWWRAP=agwwrap-$(GOOS)-$(GOARCH)

all: $(AGWWRAP)

$(AGWWRAP): ./cmd/agwwrap/main.go
	$(GO) build -o $@-$(GOOS)-$(GOARCH) ./cmd/agwwrap

install: agwwrap
	$(INSTALL) -m 755 -d $(DESTDIR)$(bindir)
	$(INSTALL) -m 755 agwwrap $(DESTDIR)$(bindir)

clean:
	rm -f $(AGWWRAP)
