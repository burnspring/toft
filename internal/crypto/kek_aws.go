package crypto

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// AWS KMS as the key-encrypting key.
//
// The URI names the key and nothing else — an ARN, by alias or by key id:
//
//	aws-kms://arn:aws:kms:us-east-1:123456789012:alias/toft
//	aws-kms://arn:aws:kms:us-east-1:123456789012:key/1234abcd-…
//
// The region comes out of the ARN rather than the environment, because a URI
// that needs a second variable to mean anything is a configuration that can be
// half-applied. Credentials come from the ambient chain — an instance profile
// on EC2, IRSA in EKS — and never from the URI.

type awsKMS struct {
	client *kms.Client
	keyID  string
	uri    string
}

func newAWSKMS(ctx context.Context, uri string) (KEK, error) {
	arn := strings.TrimPrefix(uri, SchemeAWSKMS)
	if arn == "" {
		return nil, fmt.Errorf("ENCRYPTION_KEY_URI: %s needs a key ARN after it", SchemeAWSKMS)
	}

	region, err := regionFromARN(arn)
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("ENCRYPTION_KEY_URI: loading AWS credentials: %w", err)
	}

	return &awsKMS{client: kms.NewFromConfig(cfg), keyID: arn, uri: uri}, nil
}

// regionFromARN reads the region field of an ARN — arn:partition:service:region:…
func regionFromARN(arn string) (string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) < 5 || parts[0] != "arn" || parts[3] == "" {
		return "", fmt.Errorf("ENCRYPTION_KEY_URI: %q is not a KMS key ARN — it should look like "+
			"arn:aws:kms:<region>:<account>:alias/<name> or …:key/<id>", arn)
	}
	return parts[3], nil
}

func (a *awsKMS) URI() string { return a.uri }

func (a *awsKMS) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	out, err := a.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     &a.keyID,
		Plaintext: dek,
	})
	if err != nil {
		return nil, err
	}
	return out.CiphertextBlob, nil
}

func (a *awsKMS) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	// KeyId is passed on decrypt as well as encrypt. Without it KMS will use
	// whatever key the blob names, which means a blob wrapped under a key this
	// install no longer intends to use would still open — and the grant, not
	// the blob, is meant to be the control.
	out, err := a.client.Decrypt(ctx, &kms.DecryptInput{
		KeyId:          &a.keyID,
		CiphertextBlob: wrapped,
	})
	if err != nil {
		return nil, err
	}
	return out.Plaintext, nil
}
