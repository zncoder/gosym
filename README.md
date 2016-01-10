# gosym

Similar to godef, gosym prints the location of the definition of the symbol in
the Go source code.

It is based on `go/types` and `golang.org/x/tools/go/loader`, so it does not
have the known limitations of godef. It uses `go/build` to find Go source code
files with the correct `GOOS` and `GOARCH`.

## Usage

The minimal go version is go1.5, since gosym uses the `go/types` in the standard
library.

To build,

```go get github.com/zncoder/gosym```

For example, to find the definition of the symbol at position `100` in the file `foo.go`,

```gosym -f foo.go -o 100```

You can replace godef with gosym by symbolic linking gosym to godef. This should
work with go-mode.el in Emacs.

## TODO

Gosym is slower than godef because of the use of loader.
