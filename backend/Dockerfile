# build-base provides gcc/musl-dev, which CGo needs.
# libffi-dev provides the headers that go-fitz (via
# github.com/jupiterrider/ffi) requires at build time.
# mupdf-dev ships the mupdf/ headers and a mupdf.pc
# pkg-config file consumed by go-fitz's
# `extlib pkgconfig` CGo build path.
# We build with CGo enabled because go-fitz — the MuPDF
# binding used by the PDF content parser — is CGo-only.
FROM golang:1.25-alpine AS builder
# Enable the community repo: the `mupdf-dev` package
# lives there, and go-fitz's CGo build path invokes
# `pkg-config mupdf` to discover the system MuPDF.
RUN sed -i 's/^#\(.*community\)/\1/' /etc/apk/repositories
RUN apk --no-cache add build-base libffi-dev mupdf-dev pkgconf
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build tags: extlib pkgconfig tell go-fitz to link
# against the system mupdf (via pkg-config) rather than
# the glibc-built static archives it ships under libs/.
# Those vendored archives are not safe to link on musl:
# they reference glibc-only __*_chk fortified symbols
# (__snprintf_chk, __memcpy_chk, …) that musl's libc
# does not provide. With extlib pkgconfig, go-fitz's
# CGo path issues `#cgo pkg-config: mupdf` and links
# against the Alpine-built libmupdf.so instead.
RUN CGO_ENABLED=1 go build -tags "extlib pkgconfig" -o /app/bin/okt ./cmd/app/

# mupdf-libs is the runtime dependency for the CGo
# build of go-fitz: it ships libmupdf.so and its
# transitive runtime deps (libjpeg, freetype, harfbuzz,
# jbig2dec, gumbo, mujs, openjpeg, etc.).
# libffi is needed by github.com/jupiterrider/ffi at
# runtime (the package dlopen()s libffi.so.8 in its
# init()) — install it explicitly to be safe.
#
# The runtime base MUST stay pinned to the same Alpine
# release as the builder stage above. The go-fitz CGo
# build links against the system libmupdf.so found via
# pkg-config in the builder, so the binary encodes a
# SONAME like libmupdf.so.27.2 (MuPDF 1.27.x on Alpine
# 3.24). If the runtime stage ships a different MuPDF
# (e.g. alpine:3.21 carries MuPDF 1.24.x →
# libmupdf.so.24.10), the dynamic loader fails to
# resolve the SONAME and the binary aborts at startup
# with "Error loading shared library libmupdf.so.27.2:
# No such file or directory" followed by a cascade of
# "Error relocating ./okt: fz_*: symbol not found". The
# build stage and the runtime stage therefore pull
# mupdf-libs from the same Alpine version.
FROM alpine:3.24
RUN sed -i 's/^#\(.*community\)/\1/' /etc/apk/repositories
RUN apk --no-cache add ca-certificates mupdf-libs libffi
WORKDIR /app
COPY --from=builder /app/bin/okt .
COPY --from=builder /app/configs ./configs
ENTRYPOINT ["./okt"]
