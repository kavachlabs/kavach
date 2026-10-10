module github.com/kavachlabs/kavach/examples/ledger/sdk

go 1.23.8

require (
	github.com/kavachlabs/kavach v0.0.0
	github.com/twmb/franz-go v1.19.5
	github.com/twmb/franz-go/pkg/kfake v0.0.0-20250603004440-37eecbb8927f
)

require (
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.22 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.11.2 // indirect
	golang.org/x/crypto v0.38.0 // indirect
)

replace github.com/kavachlabs/kavach => ../../..
