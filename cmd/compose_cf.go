package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	cf "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/awslabs/goformation/v7"
	gocf "github.com/awslabs/goformation/v7/cloudformation"
	gocfcf "github.com/awslabs/goformation/v7/cloudformation/cloudformation"
	"github.com/hashicorp/go-uuid"
)

const root = resourceType("template")

type bucketName string
type cleanup func()
type resourceType string
type stackName string

func main() {
	stack, template, err := validate()
	if err != nil {
		panic(err)
	}

	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		panic(err)
	}

	clientCF := cf.NewFromConfig(cfg)
	clientS3 := s3.NewFromConfig(cfg)

	if err := apply(ctx, clientCF, clientS3, stack, template); err != nil {
		panic(err)
	}
}

func validate() (stackName, *gocf.Template, error) {
	if len(os.Args) != 2 {
		return "", nil, fmt.Errorf("usage: compose_cf STACK_NAME [<stdin>]")
	}

	stack := stackName(os.Args[1])
	if len(stack) == 0 {
		return "", nil, fmt.Errorf("usage: compose_cf STACK_NAME [<stdin>]")
	}

	if stdin, err := os.Stdin.Stat(); err != nil {
		return "", nil, fmt.Errorf("unable to read CloudFormation template from stdin: %w", err)
	} else if stdin.Mode()&os.ModeNamedPipe == 0 {
		return "", nil, fmt.Errorf("unable to read CloudFormation template from stdin because stdin is not attached")
	}

	file, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", nil, fmt.Errorf("unable to read CloudFormation template from stdin: %w", err)
	}

	template, err := goformation.ParseYAML(file)
	if err != nil {
		return "", nil, fmt.Errorf("unable to parse CloudFormation template from stdin: %w", err)
	}

	if len(template.Resources) == 0 {
		return "", nil, fmt.Errorf("no resources found in CloudFormation template")
	}

	return stack, template, nil
}

func apply(ctx context.Context, clientCF *cf.Client, clientS3 *s3.Client, stack stackName, template *gocf.Template) error {
	// TODO: track down where this is coming from
	if version := gocf.NewTemplate().AWSTemplateFormatVersion; template.AWSTemplateFormatVersion != version {
		template.AWSTemplateFormatVersion = version
	}

	var cleanup []cleanup
	defer func() {
		for _, c := range cleanup {
			//goland:noinspection GoDeferInLoop
			defer c()
		}
	}()

	bucket, cleanupBucket, err := createBucket(ctx, clientS3)
	if err != nil {
		return err
	} else {
		cleanup = append(cleanup, cleanupBucket)
	}

	for resourceType, template := range splitTemplates(bucket, template) {
		if cleanupTemplate, err := uploadTemplate(ctx, clientS3, bucket, string(resourceType), template); err != nil {
			return err
		} else {
			cleanup = append(cleanup, cleanupTemplate)
		}
	}

	_, err = clientCF.DescribeStacks(ctx, &cf.DescribeStacksInput{
		StackName: aws.String(string(stack)),
	})

	if err != nil {
		return createStack(ctx, clientCF, bucket, stack)
	} else {
		return createChangeSet(ctx, clientCF, bucket, stack)
	}
}

func createBucket(ctx context.Context, client *s3.Client) (bucketName, cleanup, error) {
	key, err := uuid.GenerateUUID()
	if err != nil {
		return "", nil, err
	}

	bucket := fmt.Sprintf("com.docker.compose.%s", key)

	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return "", nil, err
	}

	return bucketName(bucket), func() {
		_, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{
			Bucket: aws.String(bucket),
		})
		if err != nil {
			fmt.Printf("Failed to remove S3 bucket: %s\n", err)
		}
	}, nil
}

func createChangeSet(ctx context.Context, client *cf.Client, bucket bucketName, stack stackName) error {
	changeSet, err := client.CreateChangeSet(ctx, &cf.CreateChangeSetInput{
		Capabilities: []cftypes.Capability{
			cftypes.CapabilityCapabilityIam,
		},
		ChangeSetName:       aws.String(fmt.Sprintf("Update-%s", time.Now().Format("2006-01-02-15-04-05Z"))),
		ChangeSetType:       cftypes.ChangeSetTypeUpdate,
		IncludeNestedStacks: aws.Bool(true),
		OnStackFailure:      cftypes.OnStackFailureRollback,
		StackName:           aws.String(string(stack)),
		TemplateURL:         aws.String(fmt.Sprintf("https://s3.amazonaws.com/%s/%s.yaml", bucket, root)),
	})
	if err != nil {
		return err
	}

	for {
		if describe, err := client.DescribeChangeSet(ctx, &cf.DescribeChangeSetInput{
			ChangeSetName:         changeSet.Id,
			IncludePropertyValues: aws.Bool(true),
			StackName:             aws.String(string(stack)),
		}); err != nil {
			return err
		} else if describe.Status == cftypes.ChangeSetStatusCreateComplete {
			break
		}
	}

	_, err = client.ExecuteChangeSet(ctx, &cf.ExecuteChangeSetInput{
		ChangeSetName: changeSet.Id,
		StackName:     aws.String(string(stack)),
	})
	if err != nil {
		return err
	}

	return nil
}

func createStack(ctx context.Context, client *cf.Client, bucket bucketName, stack stackName) error {
	_, err := client.CreateStack(ctx, &cf.CreateStackInput{
		Capabilities: []cftypes.Capability{
			cftypes.CapabilityCapabilityIam,
		},
		OnFailure:            cftypes.OnFailureRollback,
		RetainExceptOnCreate: aws.Bool(true),
		StackName:            aws.String(string(stack)),
		TemplateURL:          aws.String(fmt.Sprintf("https://s3.amazonaws.com/%s/%s.yaml", bucket, root)),
	})
	if err != nil {
		return err
	}

	return nil
}

func splitTemplates(bucket bucketName, template *gocf.Template) map[resourceType]*gocf.Template {
	nested := map[resourceType]*gocf.Template{}

	for name, resource := range template.Resources {
		resourceType := resourceType(strings.ReplaceAll(resource.AWSCloudFormationType(), "::", ""))

		if template, ok := nested[resourceType]; ok {
			template.Resources[name] = resource
		} else {
			template = gocf.NewTemplate()
			template.Resources[name] = resource
			nested[resourceType] = template
		}

		delete(template.Resources, name)
	}

	for resourceType := range nested {
		template.Resources[string(resourceType)] = &gocfcf.Stack{
			TemplateURL: fmt.Sprintf("https://s3.amazonaws.com/%s/%s.yaml", bucket, resourceType),
		}
	}

	nested[root] = template

	return nested
}

func uploadTemplate(ctx context.Context, client *s3.Client, bucket bucketName, key string, template *gocf.Template) (cleanup, error) {
	if !strings.HasSuffix(key, ".yaml") {
		key = fmt.Sprintf("%s.yaml", key)
	}

	yaml, err := template.YAML()
	if err != nil {
		return nil, err
	}

	fmt.Printf("Uploading template `%s/%s`\n```yaml\n%s```\n", bucket, key, yaml)

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Body:        bytes.NewReader(yaml),
		Bucket:      aws.String(string(bucket)),
		ContentType: aws.String("application/yaml"),
		Key:         aws.String(key),
	})
	if err != nil {
		return nil, err
	}

	return func() {
		_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(string(bucket)),
			Key:    aws.String(key),
		})
		if err != nil {
			fmt.Printf("Failed to remove S3 item: %s\n", err)
		}
	}, nil
}
