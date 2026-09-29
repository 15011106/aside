SDK  := $(shell xcrun --show-sdk-path)
ROOT := $(shell pwd)
MACOS_MIN := 13.0
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

# The Swift object carries autolink hints for its imported frameworks; the
# linker only needs the SDK's Swift stub directory on its search path. The
# object is wrapped in an archive because cgo repeats LDFLAGS at link time
# and a bare .o would produce duplicate symbols.
export CGO_LDFLAGS = -L$(ROOT)/build -lkakaobridge -L$(SDK)/usr/lib/swift

.PHONY: build clean run doctor dist

# --- local dev build (host arch) ---

# -X main.bridgeID busts Go's build cache when only the Swift archive
# changed — Go does not hash externally linked libraries.
build: build/libkakaobridge.a
	go build -ldflags "-X main.bridgeID=$$(shasum build/libkakaobridge.a | cut -d' ' -f1) -X main.version=$(VERSION)" -o aside .

build/libkakaobridge.a: bridge/kakao.swift
	@mkdir -p build
	swiftc -swift-version 5 -O -parse-as-library \
		-target arm64-apple-macos$(MACOS_MIN) -emit-object $< -o build/kakao.o
	libtool -static -o $@ build/kakao.o

doctor: build
	./aside doctor

run: build
	./aside

clean:
	rm -rf build aside dist

# --- distribution: universal (arm64 + x86_64) binary + installer zip ---

dist: clean
	@echo "› building arm64 slice"
	@$(MAKE) --no-print-directory _slice ARCH=arm64
	@echo "› building x86_64 slice"
	@$(MAKE) --no-print-directory _slice ARCH=x86_64
	@echo "› fusing universal binary"
	@mkdir -p dist/aside
	@lipo -create build/aside-arm64 build/aside-x86_64 -output dist/aside/aside
	@lipo -info dist/aside/aside
	@cp dist-assets/install.sh dist/aside/install.sh
	@cp dist-assets/README.txt dist/aside/README.txt
	@chmod +x dist/aside/install.sh
	@cd dist && zip -qr aside-$(VERSION)-macos.zip aside && echo "› wrote dist/aside-$(VERSION)-macos.zip"
	@echo "› restoring local dev build"
	@$(MAKE) --no-print-directory build

# one architecture slice: swift archive + cgo cross-build
_slice:
	@mkdir -p build
	swiftc -swift-version 5 -O -parse-as-library \
		-target $(ARCH)-apple-macos$(MACOS_MIN) -emit-object bridge/kakao.swift -o build/kakao-$(ARCH).o
	libtool -static -o build/libkakaobridge-$(ARCH).a build/kakao-$(ARCH).o
	CGO_ENABLED=1 GOARCH=$(GOARCH_$(ARCH)) \
		CC="clang -arch $(ARCH) -mmacosx-version-min=$(MACOS_MIN)" \
		CGO_LDFLAGS="-L$(ROOT)/build -lkakaobridge-$(ARCH) -L$(SDK)/usr/lib/swift" \
		go build -ldflags "-X main.bridgeID=$(ARCH) -X main.version=$(VERSION)" -o build/aside-$(ARCH) .

GOARCH_arm64  := arm64
GOARCH_x86_64 := amd64
