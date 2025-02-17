package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var version = "development"

type RpmToolingCmdOpts struct {
	AwsSecretKey string
	AwsAccessKey string
	AwsRegion    string
	Bucket       string
	BucketPrefix string
	Sign         bool
	SignPassword string
}

var rpmToolingCmdOpts RpmToolingCmdOpts
var rpms []string

func main() {
	cmd := &cobra.Command{
		Use:     "rpm_tooling",
		Short:   "Generate backport issues and cherry pick commits to branches",
		Long:    "The backport utility needs to be executed inside the repository you want to perform the actions",
		RunE:    rpmTooling,
		Version: version,
	}

	cmd.Flags().StringVar(&rpmToolingCmdOpts.AwsAccessKey, "aws-access-key", os.Getenv("AWS_ACCESS_KEY"), "")
	cmd.Flags().StringVar(&rpmToolingCmdOpts.AwsSecretKey, "aws-secret-key", os.Getenv("AWS_SECRET_KEY"), "")
	cmd.Flags().StringVar(&rpmToolingCmdOpts.AwsRegion, "aws-region", "us-east-1", "")
	cmd.Flags().StringVar(&rpmToolingCmdOpts.Bucket, "bucket", "", "")
	cmd.Flags().StringVar(&rpmToolingCmdOpts.BucketPrefix, "prefix", "", "")
	cmd.Flags().BoolVar(&rpmToolingCmdOpts.Sign, "sign", false, "")
	cmd.Flags().StringVar(&rpmToolingCmdOpts.SignPassword, "sign-pass", "", "")

	if err := cmd.Execute(); err != nil {
		logrus.Fatal(err)
	}
}

func rpmTooling(cmd *cobra.Command, args []string) error {
	for _, v := range args {
		if strings.Contains(v, ".rpm") {
			rpms = append(rpms, v)
		}
	}

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(rpmToolingCmdOpts.AwsRegion),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(rpmToolingCmdOpts.AwsAccessKey, rpmToolingCmdOpts.AwsSecretKey, "")))
	if err != nil {
		log.Fatalf("unable to load SDK config, %v", err)
	}

	prefixWithRepodata := fmt.Sprintf("%s/%s", rpmToolingCmdOpts.BucketPrefix, "repodata")

	s3Client := s3.NewFromConfig(cfg)
	input := &s3.ListObjectsV2Input{
		Bucket: &rpmToolingCmdOpts.Bucket,
		Prefix: &prefixWithRepodata,
	}

	_ = fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", rpmToolingCmdOpts.Bucket, rpmToolingCmdOpts.AwsRegion, rpmToolingCmdOpts.BucketPrefix)

	result, err := s3Client.ListObjectsV2(ctx, input)
	var noBucket *types.NoSuchBucket
	if errors.As(err, &noBucket) {
		log.Println("creating repodata and a new repo")
	} else if err != nil {
		log.Fatalf("unable to list objects from %s/%s: %v", rpmToolingCmdOpts.Bucket, rpmToolingCmdOpts.BucketPrefix, err)
	}

	mainDir, err := os.MkdirTemp("", "rpm-")
	if err != nil {
		log.Fatalf("Unable to create temp directory for rpms: %v", err)
	}

	fmt.Printf("Created temp dir at: %s\n", mainDir)

	if result.Contents == nil {
		for _, v := range rpms {
			original, err := os.Open(v)
			if err != nil {
				log.Fatalf("Unable to open file in %s: %v", v, err)
			}
			defer original.Close()

			newFile, err := os.Create(fmt.Sprintf("%s/%s", mainDir, filepath.Base(v)))
			if err != nil {
				log.Fatalf("Unable to create file in new dir in %s: %v", mainDir, err)
			}
			defer newFile.Close()

			_, err = io.Copy(newFile, original)
			if err != nil {
				log.Fatalf("Unable to copy content from file %s to %s: %v", original.Name(), newFile.Name(), err)
			}
		}

		if err := os.Mkdir(fmt.Sprintf("%s/%s", mainDir, "repodata"), 0o777); err != nil {
			log.Fatalf("Unable to create %s/repodata: %v", mainDir, "repodata")
		}

	} else if len(result.Contents) > 0 {
		for i, v := range result.Contents {
			fmt.Println("------------------------------------------------------")
			fmt.Println("index: ", i)
			fmt.Printf("content: %s\n", *v.Key)
			fmt.Println("------------------------------------------------------")
		}

		newDir, err := os.MkdirTemp(mainDir, "new_repo")
		if err != nil {
			log.Fatalf("Unable to create a temp directory: %v", err)
		}

		mergeDir, err := os.MkdirTemp(mainDir, "merge_repo")
		if err != nil {
			log.Fatalf("Unable to create a temp directory: %v", err)
		}

		oldDir, err := os.MkdirTemp(mainDir, "old_repo")
		if err != nil {
			log.Fatalf("Unable to create a temp directory: %v", err)
		}

		fmt.Println("------------------------------------------------------")
		fmt.Printf("temp dir: %s\n", mainDir)
		fmt.Printf("new dir: %s\n", newDir)
		fmt.Printf("old dir: %s\n", oldDir)
		fmt.Printf("merge dir: %s\n", mergeDir)
		fmt.Println("------------------------------------------------------")

		// newDir, err := os.MkdirTemp(mainDir, "new_repo")
		// if err != nil {
		// 	log.Fatalf("Unable to create a temp directory: %v", err)
		// }
		//
		// mergeDir, err := os.MkdirTemp(mainDir, "merge_repo")
		// if err != nil {
		// 	log.Fatalf("Unable to create a temp directory: %v", err)
		// }
		//
		// oldDir, err := os.MkdirTemp(mainDir, "merge_repo")
		// if err != nil {
		// 	log.Fatalf("Unable to create a temp directory: %v", err)
		// }

		return nil
	}

	// newDir, err := os.MkdirTemp("", "rpm-")
	// if err != nil {
	// 	log.Fatalf("Unable to create a temp directory: %v", err)
	// }
	//
	// mergeDir, err := os.MkdirTemp("", "rpm-")
	// if err != nil {
	// 	log.Fatalf("Unable to create a temp directory: %v", err)
	// }
	//
	// oldDir, err := os.MkdirTemp("", "rpm-")
	// if err != nil {
	// 	log.Fatalf("Unable to create a temp directory: %v", err)
	// }

	return nil
}
