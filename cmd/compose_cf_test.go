package main

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/awslabs/goformation/v7"
	"github.com/stretchr/testify/assert"
	"github.com/testcontainers/testcontainers-go/modules/localstack"
)

func TestUp(t *testing.T) {
	ctx, clientCF, clientS3 := setup(context.TODO(), t)

	template, err := goformation.ParseYAML([]byte(`
AWSTemplateFormatVersion: 2010-09-09
Resources:
  SomeBucket:
    Type: AWS::S3::Bucket
`))
	assert.NoError(t, err)

	err = up(ctx, clientCF, clientS3, "some_stack", template)

	assert.NoError(t, err)
}

func setup(ctx context.Context, t *testing.T) (context.Context, *cloudformation.Client, *s3.Client) {
	container, err := localstack.RunContainer(ctx)

	assert.NoError(t, err)
	if err != nil {
		panic(err)
	}

	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			panic(err)
		}
	})

	endpoint, err := container.PortEndpoint(ctx, "4566/tcp", "http")

	assert.NoError(t, err)
	if err != nil {
		panic(err)
	}

	cfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("local", "local", "local")),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(func(service, region string, opts ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{
				PartitionID:   "aws",
				URL:           endpoint,
				SigningRegion: region,
				Source:        aws.EndpointSourceCustom,
			}, nil
		})),
		config.WithRegion("us-east-1"))

	assert.NoError(t, err)
	if err != nil {
		panic(err)
	}

	return ctx, cloudformation.NewFromConfig(cfg), s3.NewFromConfig(cfg, func(options *s3.Options) {
		options.UsePathStyle = true
	})
}
