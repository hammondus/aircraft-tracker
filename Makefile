BIN     := aircraft-tracker
DIST    := dist
SERVICE := tracker

# --- map tiles ---------------------------------------------------------------
# Protomaps purges daily planet builds after roughly a week, so PLANET will 404
# eventually. When it does, pick a current date from build.protomaps.com; the
# extract only needs rebuilding when we want fresher map data.
PLANET  := https://build.protomaps.com/20260810.pmtiles
TILES   := tiles/australia.pmtiles
# Mainland + Tasmania. Widen to 96,-44,169,-9 for Lord Howe/Norfolk/Cocos.
BBOX    := 112.9,-43.7,153.7,-10.6
MAXZOOM := 14

# -trimpath strips local filesystem paths from the binary; -s -w drop the symbol
# table and DWARF info. Together they make the artifact smaller and reproducible.
LDFLAGS := -s -w
GOFLAGS := -trimpath

.PHONY: build test run release clean docker-build deploy logs tiles

build:
	go build $(GOFLAGS) -o $(BIN) .

test:
	go vet ./...
	go test ./...

run: build
	./$(BIN) -config config.json

# Deploy target is a linux/arm64 box behind nginx proxy manager.
# CGO_ENABLED=0 gives a static binary with no libc dependency, so it runs on
# any base image including scratch/alpine.
release: clean
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(DIST)/$(BIN)-linux-arm64 .

# ~1 GB, gitignored. Reads the planet's tile directory and range-requests only
# the byte ranges inside BBOX, so this transfers ~1 GB rather than the 137 GB
# the source archive weighs.
tiles:
	mkdir -p $(dir $(TILES))
	pmtiles extract $(PLANET) $(TILES) --bbox=$(BBOX) --maxzoom=$(MAXZOOM)

# Deliberately does NOT remove $(TILES) -- refetching a gigabyte because someone
# ran `make clean` is not a tradeoff worth making. Delete it by hand.
clean:
	rm -f $(BIN)
	rm -rf $(DIST)

docker-build:
	docker compose build

# Run on the server, not the dev machine.
deploy:
	git pull
	docker compose up -d --build

logs:
	docker compose logs -f $(SERVICE)
