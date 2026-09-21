prefix=/usr
bindir=$(prefix)/bin

GO=go
INSTALL=install

GOARCH=$(shell go env GOARCH)
GOOS=$(shell go env GOOS)

export GOARCH
export GOOS

AGWLISTEN=agwlisten-$(GOOS)-$(GOARCH)
AGWCONNECT=agwconnect-$(GOOS)-$(GOARCH)

all: $(AGWLISTEN) $(AGWCONNECT)

$(AGWLISTEN): $(wildcard cmd/agwlisten/*.go internal/*/*.go)
	$(GO) build -o $@ ./cmd/agwlisten

$(AGWCONNECT): $(wildcard cmd/agwconnect/*.go internal/*/*.go)
	$(GO) build -o $@ ./cmd/agwconnect

install: $(AGWLISTEN) $(AGWCONNECT)
	$(INSTALL) -m 755 -d $(DESTDIR)$(bindir)
	$(INSTALL) -m 755 $(AGWLISTEN) $(DESTDIR)$(bindir)/agwlisten
	$(INSTALL) -m 755 $(AGWCONNECT) $(DESTDIR)$(bindir)/agwconnect

clean:
	rm -f $(AGWLISTEN) $(AGWCONNECT)
