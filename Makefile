prefix=/usr
bindir=$(prefix)/bin

GO=go
INSTALL=install

GOARCH=$(shell go env GOARCH)
GOOS=$(shell go env GOOS)

export GOARCH
export GOOS

AGWLISTEN=agwlisten-$(GOOS)-$(GOARCH)

all: $(AGWLISTEN)

$(AGWLISTEN): ./cmd/agwlisten/main.go
	$(GO) build -o $@-$(GOOS)-$(GOARCH) ./cmd/agwlisten

install: $(AGWLISTEN)
	$(INSTALL) -m 755 -d $(DESTDIR)$(bindir)
	$(INSTALL) -m 755 agwlisten $(DESTDIR)$(bindir)

clean:
	rm -f $(AGWLISTEN)
