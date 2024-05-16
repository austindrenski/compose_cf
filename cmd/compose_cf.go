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

	if err := up(ctx, clientCF, clientS3, stack, template); err != nil {
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

func up(ctx context.Context, clientCF *cf.Client, clientS3 *s3.Client, stack stackName, template *gocf.Template) error {
	bucket, cleanupBucket, err := createBucket(ctx, clientS3)
	if err != nil {
		return err
	}
	defer cleanupBucket()

	templateUrl, cleanupTemplates, err := splitTemplates(ctx, clientS3, bucket, template)
	defer cleanupTemplates()
	if err != nil {
		return err
	}

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
	fmt.Printf("Create s3 bucket %q to store cloudformation template\n", bucket)

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
			fmt.Printf("Failed to remove S3 bucket: %s\n", err)
		}
	}, nil
}

func createChangeSet(ctx context.Context, clientCF *cf.Client, stack stackName, templateUrl templateUrl) error {
	changeSet, err := clientCF.CreateChangeSet(ctx, &cf.CreateChangeSetInput{
		Capabilities: []cftypes.Capability{
			cftypes.CapabilityCapabilityIam,
		},
		ChangeSetName:       aws.String(fmt.Sprintf("Update-%s", time.Now().Format("2006-01-02-15-04-05Z"))),
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

func splitTemplates(ctx context.Context, clientS3 *s3.Client, bucket bucketName, template *gocf.Template) (templateUrl, cleanup, error) {
	var cleanups []cleanup

	cleanup := func() {
		for _, c := range cleanups {
			if c != nil {
				c()
			}
		}
	}

	if c, err := splitResources(ctx, clientS3, bucket, template.Resources, template.GetAllECSServiceResources()); err != nil {
		return "", cleanup, err
	} else {
		cleanups = append(cleanups, c)
	}

	if c, err := splitResources(ctx, clientS3, bucket, template.Resources, template.GetAllECSTaskDefinitionResources()); err != nil {
		return "", cleanup, err
	} else {
		cleanups = append(cleanups, c)
	}

	if c, err := splitResources(ctx, clientS3, bucket, template.Resources, template.GetAllServiceDiscoveryServiceResources()); err != nil {
		return "", cleanup, err
	} else {
		cleanups = append(cleanups, c)
	}

	if templateUrl, c, err := uploadTemplate(ctx, clientS3, bucket, "template.yaml", template); err != nil {
		return "", cleanup, err
	} else {
		cleanups = append(cleanups, c)
		return templateUrl, cleanup, nil
	}
}

func splitResources[T gocf.Resource](ctx context.Context, clientS3 *s3.Client, bucket bucketName, resources gocf.Resources, subset map[string]T) (cleanup, error) {
	if len(subset) == 0 {
		return nil, nil
	}

	nested := gocf.NewTemplate()

	for name, resource := range subset {
		delete(resources, name)

		nested.Resources[name] = resource

		if nested.Description == "" {
			nested.Description = fmt.Sprintf("%sNestedStack", regexp.MustCompile("[^a-zA-Z0-9]+").ReplaceAllString(resource.AWSCloudFormationType(), ""))
		}
	}

	templateUrl, cleanup, err := uploadTemplate(ctx, clientS3, bucket, fmt.Sprintf("template.%s.yaml", nested.Description), nested)
	if err != nil {
		return nil, err
	}

	resources[nested.Description] = &gocfcf.Stack{
		TemplateURL: string(templateUrl),
	}

	return cleanup, nil
}

func uploadTemplate(ctx context.Context, clientS3 *s3.Client, bucket bucketName, key string, template *gocf.Template) (templateUrl, cleanup, error) {
	yaml, err := template.YAML()
	if err != nil {
		return "", nil, err
	}

	url := templateUrl(fmt.Sprintf("https://s3.amazonaws.com/%s/%s", bucket, key))

	fmt.Printf("Uploading template %s\n```yaml\n%s\n```\n", url, yaml)

	_, err = clientS3.PutObject(ctx, &s3.PutObjectInput{
		Body:        bytes.NewReader(yaml),
		Bucket:      aws.String(string(bucket)),
		ContentType: aws.String("application/yaml"),
		Key:         aws.String(key),
	})
	if err != nil {
		return "", nil, err
	}

	return url, func() {
		_, err := clientS3.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(string(bucket)),
			Key:    aws.String(key),
		})
		if err != nil {
			fmt.Printf("Failed to remove S3 item: %s\n", err)
		}
	}, nil
}
