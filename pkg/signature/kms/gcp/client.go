//
// Copyright 2021 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gcp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"regexp"
	"time"

	gcpkms "cloud.google.com/go/kms/apiv1"
	kmspb "google.golang.org/genproto/googleapis/cloud/kms/v1"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/ReneKroon/ttlcache/v2"
	"github.com/pkg/errors"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/options"
)

//nolint:golint
const (
	Algorithm_ECDSA_P256_SHA256        = "ecdsa-p256-sha256"
	Algorithm_ECDSA_P384_SHA384        = "ecdsa-p384-sha384"
	Algorithm_RSA_PKCS1v15_2048_SHA256 = "rsa-pkcs1v15-2048-sha256"
	Algorithm_RSA_PKCS1v15_3072_SHA256 = "rsa-pkcs1v15-3072-sha256"
	Algorithm_RSA_PKCS1v15_4096_SHA256 = "rsa-pkcs1v15-4096-sha256"
	Algorithm_RSA_PKCS1v15_4096_SHA512 = "rsa-pkcs1v15-4096-sha512"
	Algorithm_RSA_PSS_2048_SHA256      = "rsa-pss-2048-sha256"
	Algorithm_RSA_PSS_3072_SHA256      = "rsa-pss-3072-sha256"
	Algorithm_RSA_PSS_4096_SHA256      = "rsa-pss-4096-sha256"
	Algorithm_RSA_PSS_4096_SHA512      = "rsa-pss-4096-sha512"
)

type gcpClient struct {
	defaultCtx context.Context
	refString  string
	projectID  string
	locationID string
	keyRing    string
	keyName    string
	version    string
	kvCache    *ttlcache.Cache
	kmsClient  *gcpkms.KeyManagementClient
}

func newGCPClient(ctx context.Context, refStr string) (*gcpClient, error) {
	var err error
	if err = ValidReference(refStr); err != nil {
		return nil, err
	}

	if ctx == nil {
		ctx = context.Background()
	}

	g := &gcpClient{
		defaultCtx: ctx,
		refString:  refStr,
		kvCache:    ttlcache.NewCache(),
	}
	g.projectID, g.locationID, g.keyRing, g.keyName, g.version, err = parseReference(refStr)
	if err != nil {
		return nil, err
	}

	g.kmsClient, err = gcpkms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "new gcp kms client")
	}

	g.kvCache.SetLoaderFunction(g.kvCacheLoaderFunction)
	g.kvCache.SkipTTLExtensionOnHit(true)
	// prime the cache
	_, err = g.kvCache.Get(CacheKey)
	if err != nil {
		return nil, errors.Wrap(err, "initializing key version from GCP KMS")
	}
	return g, nil
}

var (
	ErrKMSReference = errors.New("kms specification should be in the format gcpkms://projects/[PROJECT_ID]/locations/[LOCATION]/keyRings/[KEY_RING]/cryptoKeys/[KEY]/versions/[VERSION]")

	re = regexp.MustCompile(`^gcpkms://projects/([^/]+)/locations/([^/]+)/keyRings/([^/]+)/cryptoKeys/([^/]+)(?:/versions/([^/]+))?$`)
)

// schemes for various KMS services are copied from https://github.com/google/go-cloud/tree/master/secrets
const ReferenceScheme = "gcpkms://"

func ValidReference(ref string) error {
	if !re.MatchString(ref) {
		return ErrKMSReference
	}
	return nil
}

func parseReference(resourceID string) (projectID, locationID, keyRing, keyName, version string, err error) {
	v := re.FindStringSubmatch(resourceID)
	if len(v) != 6 {
		err = errors.Errorf("invalid gcpkms format %q", resourceID)
		return
	}
	projectID, locationID, keyRing, keyName, version = v[1], v[2], v[3], v[4], v[5]
	return
}

type cryptoKeyVersion struct {
	CryptoKeyVersion *kmspb.CryptoKeyVersion
	SignerVerifier   signature.SignerVerifier
	HashFunc         crypto.Hash
}

// use a consistent key for cache lookups
const CacheKey = "crypto_key_version"

func (g *gcpClient) kvCacheLoaderFunction(key string) (data interface{}, ttl time.Duration, err error) {
	// if we're given an explicit version, cache this value forever
	if g.version != "" {
		ttl = time.Second * 0
	} else {
		ttl = time.Second * 300
	}
	data, err = g.keyVersionName(context.Background())

	return
}

// keyVersionName returns the first key version found for a key in KMS
func (g *gcpClient) keyVersionName(ctx context.Context) (*cryptoKeyVersion, error) {
	parent := fmt.Sprintf("projects/%s/locations/%s/keyRings/%s/cryptoKeys/%s", g.projectID, g.locationID, g.keyRing, g.keyName)

	parentReq := &kmspb.GetCryptoKeyRequest{
		Name: parent,
	}
	key, err := g.kmsClient.GetCryptoKey(ctx, parentReq)
	if err != nil {
		return nil, err
	}
	if key.Purpose != kmspb.CryptoKey_ASYMMETRIC_SIGN {
		return nil, errors.New("specified key cannot be used to sign")
	}

	// if g.version was specified, use it explicitly
	var kv *kmspb.CryptoKeyVersion
	if g.version != "" {
		req := &kmspb.GetCryptoKeyVersionRequest{
			Name: parent + fmt.Sprintf("/cryptoKeyVersions/%s", g.version),
		}
		kv, err = g.kmsClient.GetCryptoKeyVersion(ctx, req)
		if err != nil {
			return nil, err
		}
	} else {
		req := &kmspb.ListCryptoKeyVersionsRequest{
			Parent:  parent,
			Filter:  "state=ENABLED",
			OrderBy: "name desc",
		}
		iterator := g.kmsClient.ListCryptoKeyVersions(ctx, req)

		// pick the key version that is enabled with the greatest version value
		kv, err = iterator.Next()
		if err != nil {
			return nil, errors.Wrap(err, "unable to find an enabled key version in GCP KMS")
		}
	}
	// kv is keyVersion to use
	crv := cryptoKeyVersion{
		CryptoKeyVersion: kv,
	}

	pubKey, err := g.fetchPublicKey(ctx, kv.Name)
	if err != nil {
		return nil, errors.Wrap(err, "unable to fetch public key while creating signer")
	}

	var rsaPriv *rsa.PrivateKey
	var ecPriv *ecdsa.PrivateKey

	switch kv.Algorithm {
	case kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256:
	case kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384:
		ecPub := pubKey.(*ecdsa.PublicKey)
		ecPriv = &ecdsa.PrivateKey{PublicKey: *ecPub}
	case kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256, kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_3072_SHA256,
		kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA256, kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA512,
		kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256, kmspb.CryptoKeyVersion_RSA_SIGN_PSS_3072_SHA256,
		kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA256, kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA512:
		rsaPub := pubKey.(*rsa.PublicKey)
		rsaPriv = &rsa.PrivateKey{PublicKey: *rsaPub}
	default:
		return nil, errors.New("unknown algorithm specified by KMS")
	}

	// crv.SignerVerifier is set here to enable storing the public key & hash algorithm together,
	// as well as using the in memory Verifier to perform the verify operations.
	// crv.SignerVerifier.Sign() or crv.SignerVerifier.SignMessage() should NEVER be used since the private
	// key is stored in GCP KMS, not in memory
	switch kv.Algorithm {
	case kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256:
		crv.SignerVerifier, err = signature.LoadECDSASignerVerifier(ecPriv, crypto.SHA256)
		crv.HashFunc = crypto.SHA256
	case kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384:
		crv.SignerVerifier, err = signature.LoadECDSASignerVerifier(ecPriv, crypto.SHA384)
		crv.HashFunc = crypto.SHA384
	case kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256,
		kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_3072_SHA256,
		kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA256:
		crv.SignerVerifier, err = signature.LoadRSAPKCS1v15SignerVerifier(rsaPriv, crypto.SHA256)
		crv.HashFunc = crypto.SHA256
	case kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA512:
		crv.SignerVerifier, err = signature.LoadRSAPKCS1v15SignerVerifier(rsaPriv, crypto.SHA512)
		crv.HashFunc = crypto.SHA512
	case kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256,
		kmspb.CryptoKeyVersion_RSA_SIGN_PSS_3072_SHA256,
		kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA256:
		crv.SignerVerifier, err = signature.LoadRSAPSSSignerVerifier(rsaPriv, crypto.SHA256, nil)
		crv.HashFunc = crypto.SHA256
	case kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA512:
		crv.SignerVerifier, err = signature.LoadRSAPSSSignerVerifier(rsaPriv, crypto.SHA512, nil)
		crv.HashFunc = crypto.SHA512
	default:
		return nil, errors.New("unknown algorithm specified by KMS")
	}
	if err != nil {
		return nil, errors.Wrap(err, "initializing internal verifier")
	}
	return &crv, nil
}

func (g *gcpClient) fetchPublicKey(ctx context.Context, name string) (crypto.PublicKey, error) {
	// Build the request.
	pkreq := &kmspb.GetPublicKeyRequest{Name: name}
	// Call the API.
	pk, err := g.kmsClient.GetPublicKey(ctx, pkreq)
	if err != nil {
		return nil, errors.Wrap(err, "public key")
	}
	return cryptoutils.UnmarshalPEMToPublicKey([]byte(pk.GetPem()))
}

func (g *gcpClient) getHashFunc() (crypto.Hash, error) {
	ckv, err := g.getCKV()
	if err != nil {
		return 0, err
	}
	return ckv.HashFunc, nil
}

// getCKV gets the latest CryptoKeyVersion from the client's cache, which may trigger an actual
// call to GCP if the existing entry in the cache has expired.
func (g *gcpClient) getCKV() (*cryptoKeyVersion, error) {
	// we get once and use consistently to ensure the cache value doesn't change underneath us
	kmsVersionInt, err := g.kvCache.Get(CacheKey)
	if err != nil {
		return nil, err
	}

	return kmsVersionInt.(*cryptoKeyVersion), nil
}

func (g *gcpClient) sign(ctx context.Context, digest []byte, alg crypto.Hash, crc uint32) ([]byte, error) {
	ckv, err := g.getCKV()
	if err != nil {
		return nil, err
	}

	gcpSignReq := kmspb.AsymmetricSignRequest{
		Name:   ckv.CryptoKeyVersion.Name,
		Digest: &kmspb.Digest{},
	}

	if crc != 0 {
		gcpSignReq.DigestCrc32C = wrapperspb.Int64(int64(crc))
	}

	switch alg {
	case crypto.SHA256:
		gcpSignReq.Digest.Digest = &kmspb.Digest_Sha256{
			Sha256: digest,
		}
	case crypto.SHA384:
		gcpSignReq.Digest.Digest = &kmspb.Digest_Sha384{
			Sha384: digest,
		}
	case crypto.SHA512:
		gcpSignReq.Digest.Digest = &kmspb.Digest_Sha512{
			Sha512: digest,
		}
	default:
		return nil, errors.New("unsupported hash function")
	}

	resp, err := g.kmsClient.AsymmetricSign(ctx, &gcpSignReq)
	if err != nil {
		return nil, errors.Wrap(err, "calling GCP AsymmetricSign")
	}

	// Optional, but recommended: perform integrity verification on result.
	// For more details on ensuring E2E in-transit integrity to and from Cloud KMS visit:
	// https://cloud.google.com/kms/docs/data-integrity-guidelines
	if crc != 0 && !resp.VerifiedDigestCrc32C {
		return nil, fmt.Errorf("AsymmetricSign: request corrupted in-transit")
	}
	if int64(crc32.Checksum(resp.Signature, crc32.MakeTable(crc32.Castagnoli))) != resp.SignatureCrc32C.Value {
		return nil, fmt.Errorf("AsymmetricSign: response corrupted in-transit")
	}

	return resp.Signature, nil
}

func (g *gcpClient) public(ctx context.Context) (crypto.PublicKey, error) {
	crv, err := g.getCKV()
	if err != nil {
		return nil, errors.Wrap(err, "transient error getting info from KMS")
	}
	return crv.SignerVerifier.PublicKey(options.WithContext(ctx))

}

func (g *gcpClient) verify(sig, message io.Reader, opts ...signature.VerifyOption) error {
	crv, err := g.getCKV()
	if err != nil {
		return errors.Wrap(err, "transient error getting info from KMS")
	}
	if err := crv.SignerVerifier.VerifySignature(sig, message, opts...); err != nil {
		// key could have been rotated, clear cache and try again if we're not pinned to a version
		if g.version == "" {
			_ = g.kvCache.Remove(CacheKey)
			crv, err = g.getCKV()
			if err != nil {
				return errors.Wrap(err, "transient error getting info from KMS")
			}
			return crv.SignerVerifier.VerifySignature(sig, message, opts...)
		}
		return errors.Wrap(err, "failed to verify for fixed version")
	}
	return nil
}

func (g *gcpClient) createKey(ctx context.Context, algorithm string) (crypto.PublicKey, error) {
	if err := g.createKeyRing(ctx); err != nil {
		return nil, errors.Wrap(err, "creating key ring")
	}

	getKeyRequest := &kmspb.GetCryptoKeyRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/keyRings/%s/cryptoKeys/%s", g.projectID, g.locationID, g.keyRing, g.keyName),
	}
	if _, err := g.kmsClient.GetCryptoKey(ctx, getKeyRequest); err == nil {
		return g.public(ctx)
	}

	var algorithmMap = map[string]kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm{
		Algorithm_ECDSA_P256_SHA256:        kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256,
		Algorithm_ECDSA_P384_SHA384:        kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384,
		Algorithm_RSA_PKCS1v15_2048_SHA256: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256,
		Algorithm_RSA_PKCS1v15_3072_SHA256: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_3072_SHA256,
		Algorithm_RSA_PKCS1v15_4096_SHA256: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA256,
		Algorithm_RSA_PKCS1v15_4096_SHA512: kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA512,
		Algorithm_RSA_PSS_2048_SHA256:      kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256,
		Algorithm_RSA_PSS_3072_SHA256:      kmspb.CryptoKeyVersion_RSA_SIGN_PSS_3072_SHA256,
		Algorithm_RSA_PSS_4096_SHA256:      kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA256,
		Algorithm_RSA_PSS_4096_SHA512:      kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA512,
	}

	if _, ok := algorithmMap[algorithm]; !ok {
		return nil, errors.New("unknown algorithm requested")
	}

	createKeyRequest := &kmspb.CreateCryptoKeyRequest{
		Parent:      fmt.Sprintf("projects/%s/locations/%s/keyRings/%s", g.projectID, g.locationID, g.keyRing),
		CryptoKeyId: g.keyName,
		CryptoKey: &kmspb.CryptoKey{
			Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{
				Algorithm: algorithmMap[algorithm],
			},
		},
	}
	if _, err := g.kmsClient.CreateCryptoKey(ctx, createKeyRequest); err != nil {
		return nil, errors.Wrap(err, "creating crypto key")
	}
	return g.public(ctx)
}

func (g *gcpClient) createKeyRing(ctx context.Context) error {
	getKeyRingRequest := &kmspb.GetKeyRingRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/keyRings/%s", g.projectID, g.locationID, g.keyRing),
	}
	if result, err := g.kmsClient.GetKeyRing(ctx, getKeyRingRequest); err == nil {
		log.Printf("Key ring %s already exists in GCP KMS, moving on to creating key.\n", result.GetName())
		// key ring already exists, no need to create
		return nil
	}
	// try to create key ring
	createKeyRingRequest := &kmspb.CreateKeyRingRequest{
		Parent:    fmt.Sprintf("projects/%s/locations/%s", g.projectID, g.locationID),
		KeyRingId: g.keyRing,
	}
	result, err := g.kmsClient.CreateKeyRing(ctx, createKeyRingRequest)
	log.Printf("Created key ring %s in GCP KMS.\n", result.GetName())
	return err
}