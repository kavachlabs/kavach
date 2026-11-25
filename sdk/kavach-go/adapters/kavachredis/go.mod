module github.com/kavachlabs/kavach-go/adapters/kavachredis

go 1.23

require (
	github.com/kavachlabs/kavach-go v0.2.0
	github.com/redis/go-redis/v9 v9.17.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
)

replace github.com/kavachlabs/kavach-go => ../..
