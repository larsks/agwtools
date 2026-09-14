prefix=/usr
bindir=$(prefix)/bin

GO=go
INSTALL=install

all: agwwrap

agwwrap:
	$(GO) build -o $@ ./cmd/$@

install: agwwrap
	$(INSTALL) -m 755 -d $(DESTDIR)$(bindir)
	$(INSTALL) -m 755 agwwrap $(DESTDIR)$(bindir)

clean:
	rm -f agwwrap
