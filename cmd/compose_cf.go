package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
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

type bucketName string
type changeSetName string
type cleanup func()
type stackName string
type templateUrl string

func main() {
	name, template := validateArgs(os.Args)

	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		panic(err)
	}

	clientCF := cf.NewFromConfig(cfg)
	clientS3 := s3.NewFromConfig(cfg)

	if err := up(ctx, clientCF, clientS3, name, template); err != nil {
		panic(err)
	}
}

func validateArgs(args []string) (stackName, *gocf.Template) {
	if len(args) != 2 {
		panic("usage: compose_cf CF_STACK_NAME CF_TEMPLATE_PATH")
	}

	name := args[1]
	if name == "" {
		panic("Must specify a stack name")
	}

	file := args[2]
	if file == "" {
		panic("Must specify a file path to an existing CloudFormation template")
	}

	template, err := goformation.Open(file)
	if err != nil {
		panic(err)
	}

	return stackName(name), template
}

func up(ctx context.Context, clientCF *cf.Client, clientS3 *s3.Client, stack stackName, template *gocf.Template) error {
	bucket, cleanupBucket, err := createBucket(ctx, clientS3)
	if err != nil {
		return err
	}
	defer cleanupBucket()

	templateUrl, cleanupTemplates, err := splitTemplates(ctx, clientS3, bucket, stack, template)
	if err != nil {
		return err
	}
	defer cleanupTemplates()

	changeSetName, err := createChangeSet(ctx, clientCF, stack, templateUrl)
	if err != nil {
		return err
	}

	_, err = clientCF.ExecuteChangeSet(ctx, &cf.ExecuteChangeSetInput{
		ChangeSetName: aws.String(string(changeSetName)),
		StackName:     aws.String(string(stack)),
	})
	if err != nil {
		return err
	}

	return nil
}

func createBucket(ctx context.Context, clientS3 *s3.Client) (bucketName, cleanup, error) {
	key, err := uuid.GenerateUUID()
	if err != nil {
		return "", nil, err
	}

	bucket := fmt.Sprintf("com.docker.compose.%s", key)
	fmt.Printf("Create s3 bucket %q to store cloudformation template", bucket)

	_, err = clientS3.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return "", nil, err
	}

	return bucketName(bucket), func() {
		_, err := clientS3.DeleteBucket(ctx, &s3.DeleteBucketInput{
			Bucket: aws.String(bucket),
		})
		if err != nil {
			fmt.Printf("Failed to remove S3 bucket: %s", err)
		}
	}, nil
}

func createChangeSet(ctx context.Context, clientCF *cf.Client, stack stackName, templateUrl templateUrl) (changeSetName, error) {
	update := fmt.Sprintf("Update%s", time.Now().Format("2024-05-02-15-04-05"))

	changeset, err := clientCF.CreateChangeSet(ctx, &cf.CreateChangeSetInput{
		ChangeSetName: aws.String(update),
		ChangeSetType: changeSetType(ctx, clientCF, stack),
		StackName:     aws.String(string(stack)),
		TemplateURL:   aws.String(string(templateUrl)),
		Capabilities: []cftypes.Capability{
			cftypes.CapabilityCapabilityIam,
		},
	})
	if err != nil {
		return "", err
	}

	describe, err := clientCF.DescribeChangeSet(ctx, &cf.DescribeChangeSetInput{
		ChangeSetName: aws.String(update),
		StackName:     aws.String(string(stack)),
	})
	if err != nil {
		return "", err
	}

	if describe.Status == cftypes.ChangeSetStatusFailed {
		return "", fmt.Errorf(*describe.StatusReason)
	}

	return changeSetName(*changeset.Id), err
}

func changeSetType(ctx context.Context, clientCF *cf.Client, stack stackName) cftypes.ChangeSetType {
	stacks, err := clientCF.DescribeStacks(ctx, &cf.DescribeStacksInput{
		StackName: aws.String(string(stack)),
	})
	if err != nil {
		return cftypes.ChangeSetTypeCreate
	}

	if len(stacks.Stacks) == 0 {
		return cftypes.ChangeSetTypeCreate
	}

	return cftypes.ChangeSetTypeUpdate
}

func splitTemplates(ctx context.Context, clientS3 *s3.Client, bucket bucketName, stack stackName, template *gocf.Template) (templateUrl, cleanup, error) {
	var cleanups []cleanup

	cleanup := func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}

	for _, resources := range getSplittableResources(template) {
		cleanupTemplate, err := splitResources(ctx, clientS3, bucket, stack, template, resources)
		if err != nil {
			cleanup()
			return "", nil, err
		}

		cleanups = append(cleanups, cleanupTemplate)
	}

	templateUrl, cleanupTemplate, err := uploadTemplate(ctx, clientS3, bucket, fmt.Sprintf("%s/template.yaml", stack), template)
	if err != nil {
		cleanup()
		return "", nil, err
	}

	cleanups = append(cleanups, cleanupTemplate)

	return templateUrl, cleanup, nil
}

func splitResources(ctx context.Context, clientS3 *s3.Client, bucket bucketName, stack stackName, template *gocf.Template, resources gocf.Resources) (cleanup, error) {
	if len(resources) == 0 {
		return nil, nil
	}

	var nested = gocf.NewTemplate()
	var nestedName string

	for key, resource := range resources {
		nested.Resources[key] = resource
		template.Resources[key] = nil

		if nestedName == "" {
			nestedName = regexp.MustCompile("[^a-zA-Z0-9]+").ReplaceAllString(resource.AWSCloudFormationType(), "")
		}
	}

	templateUrl, cleanup, err := uploadTemplate(ctx, clientS3, bucket, fmt.Sprintf("%s/template.%s.yaml", stack, nestedName), nested)
	if err != nil {
		return nil, err
	}

	template.Resources[fmt.Sprintf("%sNestedStack", nestedName)] = &gocfcf.Stack{
		TemplateURL: string(templateUrl),
	}

	return cleanup, nil
}

func uploadTemplate(ctx context.Context, clientS3 *s3.Client, bucket bucketName, key string, template *gocf.Template) (templateUrl, cleanup, error) {
	yaml, err := template.YAML()
	if err != nil {
		return "", nil, err
	}

	_, err = clientS3.PutObject(ctx, &s3.PutObjectInput{
		Key:         aws.String(key),
		Body:        bytes.NewReader(yaml),
		Bucket:      aws.String(string(bucket)),
		ContentType: aws.String("application/yaml"),
	})
	if err != nil {
		return "", nil, err
	}

	return templateUrl(fmt.Sprintf("https://s3.amazonaws.com/%s/%s", bucket, key)), func() {
		_, err := clientS3.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(string(bucket)),
			Key:    aws.String(key),
		})
		if err != nil {
			fmt.Printf("Failed to remove S3 item: %s", err)
		}
	}, nil
}

func getSplittableResources(template *gocf.Template) []gocf.Resources {
	return []gocf.Resources{
		// AWS::ECS::Service
		func(template *gocf.Template) gocf.Resources {
			resources := gocf.Resources{}

			for name, resource := range template.GetAllECSServiceResources() {
				resources[name] = resource
			}

			return resources
		}(template),
		// AWS::ECS::TaskDefinition
		func(template *gocf.Template) gocf.Resources {
			resources := gocf.Resources{}

			for name, resource := range template.GetAllECSTaskDefinitionResources() {
				resources[name] = resource
			}

			return resources
		}(template),
		// AWS::ServiceDiscovery::Service
		func(template *gocf.Template) gocf.Resources {
			resources := gocf.Resources{}

			for name, resource := range template.GetAllServiceDiscoveryServiceResources() {
				resources[name] = resource
			}

			return resources
		}(template),
	}
}
