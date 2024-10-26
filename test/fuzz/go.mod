module github.com/franchb/sigstore/test/fuzz

go 1.23.2

replace github.com/franchb/sigstore => ../../

require (
	github.com/AdaLogics/go-fuzz-headers v0.0.0-20240806141605-e8a1dd7889d6
	github.com/dvyukov/go-fuzz v0.0.0-20240924070022-e577bee5275c
	github.com/franchb/sigstore v1.8.10
	github.com/secure-systems-lab/go-securesystemslib v0.8.0
)

require (
	github.com/elazarl/go-bindata-assetfs v1.0.1 // indirect
	github.com/go-jose/go-jose/v4 v4.0.4 // indirect
	github.com/google/go-containerregistry v0.20.2 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/letsencrypt/boulder v0.0.0-20241021211548-844334e04aef // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/stephens2424/writerset v1.0.2 // indirect
	github.com/titanous/rocacheck v0.0.0-20171023193734-afe73141d399 // indirect
	golang.org/x/crypto v0.28.0 // indirect
	golang.org/x/mod v0.18.0 // indirect
	golang.org/x/sync v0.8.0 // indirect
	golang.org/x/sys v0.26.0 // indirect
	golang.org/x/term v0.25.0 // indirect
	golang.org/x/tools v0.22.0 // indirect
	google.golang.org/protobuf v1.35.1 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
