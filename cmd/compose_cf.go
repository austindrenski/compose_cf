package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
type cleanup func()
type stackName string
type templateUrl string

func main() {
	stack, template := validate()

	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		panic(err)
	}

	clientCF := cf.NewFromConfig(cfg)
	clientS3 := s3.NewFromConfig(cfg)

	if err := up(ctx, clientCF, clientS3, stack, template); err != nil {
		panic(err)
	}
}

func validate() (stackName, *gocf.Template) {
	stack, ok := os.LookupEnv("COMPOSE_CF_STACK_NAME")
	if !ok {
		panic("Required environment variable `COMPOSE_CF_STACK_NAME` not set")
	}

	file, err := io.ReadAll(os.Stdin)
	if err != nil {
		panic(fmt.Errorf("unable to read CloudFormation template from stdin: %w", err))
	}

	template, err := goformation.ParseYAML(file)
	if err != nil {
		panic(err)
	}

	return stackName(stack), template
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

	_, err = clientCF.DescribeStacks(ctx, &cf.DescribeStacksInput{
		StackName: aws.String(string(stack)),
	})

	if err != nil {
		return createStack(ctx, clientCF, stack, templateUrl)
	} else {
		return createChangeSet(ctx, clientCF, stack, templateUrl)
	}
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

func createChangeSet(ctx context.Context, clientCF *cf.Client, stack stackName, templateUrl templateUrl) error {
	changeSet, err := clientCF.CreateChangeSet(ctx, &cf.CreateChangeSetInput{
		Capabilities: []cftypes.Capability{
			cftypes.CapabilityCapabilityIam,
		},
		ChangeSetName:       aws.String(fmt.Sprintf("Update_%s", time.Now().Format("2006_01_02_15_04_05Z"))),
		ChangeSetType:       cftypes.ChangeSetTypeUpdate,
		IncludeNestedStacks: aws.Bool(true),
		OnStackFailure:      cftypes.OnStackFailureRollback,
		StackName:           aws.String(string(stack)),
		TemplateURL:         aws.String(string(templateUrl)),
	})
	if err != nil {
		return err
	}

	describe, err := clientCF.DescribeChangeSet(ctx, &cf.DescribeChangeSetInput{
		ChangeSetName: changeSet.Id,
		StackName:     aws.String(string(stack)),
	})
	if err != nil {
		return err
	}

	if describe.Status == cftypes.ChangeSetStatusFailed {
		return fmt.Errorf(*describe.StatusReason)
	}

	_, err = clientCF.ExecuteChangeSet(ctx, &cf.ExecuteChangeSetInput{
		ChangeSetName: changeSet.Id,
		StackName:     aws.String(string(stack)),
	})
	if err != nil {
		return err
	}

	return nil
}

func createStack(ctx context.Context, clientCF *cf.Client, stack stackName, templateUrl templateUrl) error {
	_, err := clientCF.CreateStack(ctx, &cf.CreateStackInput{
		Capabilities: []cftypes.Capability{
			cftypes.CapabilityCapabilityIam,
		},
		OnFailure:            cftypes.OnFailureRollback,
		RetainExceptOnCreate: aws.Bool(true),
		StackName:            aws.String(string(stack)),
		TemplateURL:          aws.String(string(templateUrl)),
	})
	if err != nil {
		return err
	}

	return nil
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

	templateUrl, cleanupTemplate, err := uploadTemplate(ctx, clientS3, bucket, stack, "template.yaml", template)
	if err != nil {
		cleanup()
		return "", nil, err
	}

	cleanups = append(cleanups, cleanupTemplate)

	return templateUrl, cleanup, nil
}

func splitResources(ctx context.Context, clientS3 *s3.Client, bucket bucketName, stack stackName, template *gocf.Template, resources gocf.Resources) (cleanup, error) {
	if len(resources) == 0 {
		return func() {}, nil
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

	templateUrl, cleanup, err := uploadTemplate(ctx, clientS3, bucket, stack, fmt.Sprintf("template.%s.yaml", nestedName), nested)
	if err != nil {
		return nil, err
	}

	template.Resources[fmt.Sprintf("%sNestedStack", nestedName)] = &gocfcf.Stack{
		TemplateURL: string(templateUrl),
	}

	return cleanup, nil
}

func uploadTemplate(ctx context.Context, clientS3 *s3.Client, bucket bucketName, stack stackName, name string, template *gocf.Template) (templateUrl, cleanup, error) {
	yaml, err := template.YAML()
	if err != nil {
		return "", nil, err
	}

	_, err = clientS3.PutObject(ctx, &s3.PutObjectInput{
		Key:         aws.String(fmt.Sprintf("%s/%s", stack, name)),
		Body:        bytes.NewReader(yaml),
		Bucket:      aws.String(string(bucket)),
		ContentType: aws.String("application/yaml"),
	})
	if err != nil {
		return "", nil, err
	}

	return templateUrl(fmt.Sprintf("https://s3.amazonaws.com/%s/%s/%s", bucket, stack, name)), func() {
		_, err := clientS3.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(string(bucket)),
			Key:    aws.String(fmt.Sprintf("%s/%s", stack, name)),
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
