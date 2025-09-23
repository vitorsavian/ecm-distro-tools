package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const (
	newRepoPath    = "/tmp/new_repo"
	oldRepoPath    = "/tmp/old_repo"
	mergedRepoPath = "/tmp/merged_repo"
	version        = "development"
)

type RpmCmdOpts struct {
	Bucket       string
	Prefix       string
	Visibility   string
	AwsAccessKey string
	AwsSecretKey string
	Sign         bool
	SignPass     string
	RpmFiles     []string
	Rebuild      bool
}

var rpmCmdOpts RpmCmdOpts

func signRepo(password, repoPath string) error {
	repomdPath := fmt.Sprintf("%s/repodata/repomd.xml", repoPath)

	if password != "" {
		command := fmt.Sprintf(`
expect -c '
set timeout 60
spawn gpg --pinentry-mode loopback --force-v3-sigs --verbose --detach-sign --armor %s
expect -re "Enter passphrase.*"
send -- "%s\r"
expect eof
lassign [wait] _ _ _ code
exit $code
'
`, repoPath, password)

		logrus.Infof("Signing %s (interactive passphrase).", repomdPath)
		cmd := exec.Command("bash", "-c", command)
		return cmd.Run()
	} else {
		logrus.Infof("Signing %s (interactive passphrase).", repomdPath)
		cmd := exec.Command("gpg", "--detach-sign", "--armor", repomdPath)
		return cmd.Run()
	}
}

func sign(password, rpmPath string) error {
	if password != "" {
		command := fmt.Sprintf(`
expect -c '
set timeout 60
spawn rpmsign --addsign %s
expect -re "Enter passphrase.*"
send -- "%s\r"
expect eof
lassign [wait] _ _ _ code
exit $code
'
`, rpmPath, password)
		cmd := exec.Command("bash", "-c", command)
		return cmd.Run()
	} else {
		logrus.Infof("Signing %s (interactive passphrase).", rpmPath)
		cmd := exec.Command("rpm", "--addsign", rpmPath)
		return cmd.Run()
	}
}

func createS3Client(accessKey, secretKey string) (*s3.Client, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		config.WithDefaultRegion("us-east-1"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %v", err)
	}

	return s3.NewFromConfig(cfg), nil
}

func uploadS3Object(client *s3.Client, bucket, key, localPath string, visibility string) error {
	ctx := context.TODO()

	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", localPath, err)
	}
	defer file.Close()

	input := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   file,
	}

	if visibility == "public" {
		input.ACL = types.ObjectCannedACLPublicRead
	} else {
		input.ACL = types.ObjectCannedACLPrivate
	}

	_, err = client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to upload object %s: %w", key, err)
	}

	logrus.Infof("Uploaded %s -> s3://%s/%s", localPath, bucket, key)
	return nil
}

func uploadDirectory(client *s3.Client, bucket, prefix, localDir, visibility string) error {
	return filepath.WalkDir(localDir, func(path string, info os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		relativePath, err := filepath.Rel(localDir, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path: %w", err)
		}

		s3Key := relativePath
		if prefix != "" {
			s3Key = prefix + "/" + s3Key
		}

		return uploadS3Object(client, bucket, s3Key, path, visibility)
	})
}

func deleteS3Objects(client *s3.Client, bucket string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	ctx := context.TODO()

	var objectIds []types.ObjectIdentifier
	for _, key := range keys {
		objectIds = append(objectIds, types.ObjectIdentifier{
			Key: aws.String(key),
		})
	}

	input := &s3.DeleteObjectsInput{
		Bucket: aws.String(bucket),
		Delete: &types.Delete{
			Objects: objectIds,
		},
	}

	result, err := client.DeleteObjects(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to delete objects: %w", err)
	}

	logrus.Infof("Deleted %d objects from S3", len(result.Deleted))

	if len(result.Errors) > 0 {
		for _, deleteError := range result.Errors {
			logrus.Errorf("Failed to delete %s: %s", *deleteError.Key, *deleteError.Message)
		}
	}

	return nil
}

func listS3Objects(client *s3.Client, bucket, prefix string) ([]types.Object, error) {
	ctx := context.TODO()

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	}

	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}

	var objects []types.Object

	paginator := s3.NewListObjectsV2Paginator(client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}
		objects = append(objects, page.Contents...)
	}

	return objects, nil
}

func deleteS3Folder(client *s3.Client, bucket, folderPrefix string) error {
	logrus.Infof("Listing objects in folder: %s", folderPrefix)

	objects, err := listS3Objects(client, bucket, folderPrefix)
	if err != nil {
		return fmt.Errorf("failed to list objects in folder %s: %w", folderPrefix, err)
	}

	if len(objects) == 0 {
		logrus.Infof("No objects found in folder: %s", folderPrefix)
		return nil
	}

	var keys []string
	for _, obj := range objects {
		keys = append(keys, *obj.Key)
	}

	logrus.Infof("Found %d objects to delete in folder: %s", len(keys), folderPrefix)

	return deleteS3Objects(client, bucket, keys)
}

func downloadS3Object(client *s3.Client, bucket, key, localPath string) error {
	ctx := context.TODO()

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	input := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}

	result, err := client.GetObject(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to download object %s: %w", key, err)
	}
	defer result.Body.Close()

	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file %s: %w", localPath, err)
	}
	defer file.Close()

	_, err = io.Copy(file, result.Body)
	if err != nil {
		return fmt.Errorf("failed to write file %s: %w", localPath, err)
	}

	logrus.Infof("Downloaded %s -> %s", key, localPath)
	return nil
}

func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}

func rpmTool(cmd *cobra.Command, args []string) error {
	rpmCmdOpts.RpmFiles = args

	client, err := createS3Client(rpmCmdOpts.AwsAccessKey, rpmCmdOpts.AwsSecretKey)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(oldRepoPath, 0777); err != nil {
		return err
	}

	if err := os.MkdirAll(newRepoPath, 0777); err != nil {
		return err
	}

	if rpmCmdOpts.Rebuild {
		logrus.Info("Rebuild mode enabled. Clearing old, new, and merged repository directories.")
		repodata, err := listS3Objects(client, rpmCmdOpts.Bucket, rpmCmdOpts.Prefix)
		if err != nil {
			return err
		}

		if len(repodata) > 0 {
			logrus.Infof("Found %d items in S3 bucket %s with prefix %s", len(repodata), rpmCmdOpts.Bucket, rpmCmdOpts.Prefix)
			for _, item := range repodata {
				relativePath := *item.Key

				// this is for when prefix is set, mostly to avoid the need to handle newRepoPath+prefix+"/"+file in newRepoPath
				if rpmCmdOpts.Prefix != "" {
					relativePath = (*item.Key)[len(rpmCmdOpts.Prefix)+1:]
				}

				localPath := filepath.Join(newRepoPath, relativePath)
				if err := downloadS3Object(client, rpmCmdOpts.Bucket, *item.Key, localPath); err != nil {
					return err
				}
			}

			logrus.Info("Old RPMs downloaded from S3.")

			if rpmCmdOpts.Sign {
				logrus.Info("Signing new repository metadata.")
				if err := signRepo(rpmCmdOpts.SignPass, newRepoPath); err != nil {
					return err
				}
			}

			logrus.Info("Deleting old repodata from S3.")
			if err := deleteS3Folder(client, rpmCmdOpts.Bucket, rpmCmdOpts.Prefix+"repodata"); err != nil {
				return err
			}

			uploadDirectory(client, rpmCmdOpts.Bucket, rpmCmdOpts.Prefix, newRepoPath, rpmCmdOpts.Visibility)
		} else {
			logrus.Info("No existing RPMs found in S3.")
		}

		return nil
	}

	if len(rpmCmdOpts.RpmFiles) == 0 {
		return errors.New("at least one RPM file must be provided")
	}

	for _, rpmFile := range rpmCmdOpts.RpmFiles {
		if rpmCmdOpts.Sign {
			logrus.Infof("Signing %s", rpmFile)
			if err := sign(rpmCmdOpts.SignPass, rpmFile); err != nil {
				return err
			}
		}

		basename := filepath.Base(rpmFile)
		localDest := filepath.Join(newRepoPath, basename)
		logrus.Infof("Copying %s to %s", rpmFile, localDest)
		if err := copyFile(rpmFile, localDest); err != nil {
			return err
		}
	}

	logrus.Info("Running createrepo_c for new RPMs only.")
	comd := exec.Command("createrepo_c", "--checksum", "sha256", newRepoPath)
	if err := comd.Run(); err != nil {
		return err
	}

	repodataNew := filepath.Join(newRepoPath, "repodata")
	repomdNew := filepath.Join(repodataNew, "repomd.xml")

	logrus.Infof("Repodata created at: %s", repodataNew)
	logrus.Infof("Repomd.xml location: %s", repomdNew)

	repodata, err := listS3Objects(client, rpmCmdOpts.Bucket, rpmCmdOpts.Prefix+"repodata")
	if err != nil {
		return err
	}

	if len(repodata) > 0 {
		logrus.Infof("Found %d items in S3 bucket %s with prefix %s", len(repodata), rpmCmdOpts.Bucket, rpmCmdOpts.Prefix+"repodata")
		for _, item := range repodata {
			localPath := filepath.Join(oldRepoPath, "repodata")
			itemPath := filepath.Join(localPath, filepath.Base(*item.Key))
			if err := downloadS3Object(client, rpmCmdOpts.Bucket, *item.Key, itemPath); err != nil {
				return err
			}
		}

		logrus.Info("Running createrepo_c for old + new RPMs.")
		if err := os.MkdirAll(mergedRepoPath, 0777); err != nil {
			return err
		}

		mergeRepoScriptCmd := exec.Command("mergerepo_c",
			"--repo="+oldRepoPath,
			"--repo="+newRepoPath,
			"--all",
			"--omit-baseurl",
			"-o", mergedRepoPath)

		if err := mergeRepoScriptCmd.Run(); err != nil {
			return fmt.Errorf("failed to merge repositories: %w", err)
		}

		repodataMerged := filepath.Join(mergedRepoPath, "repodata")
		repomdMerged := filepath.Join(repodataMerged, "repomd.xml")

		logrus.Infof("Merged repodata created at: %s", repodataMerged)
		logrus.Infof("Merged repomd.xml location: %s", repomdMerged)

		if rpmCmdOpts.Sign {
			logrus.Info("Signing merged repository metadata.")
			if err := signRepo(rpmCmdOpts.SignPass, mergedRepoPath); err != nil {
				return err
			}
		}

		logrus.Info("Deleting old repodata from S3.")
		if err := deleteS3Folder(client, rpmCmdOpts.Bucket, rpmCmdOpts.Prefix+"repodata"); err != nil {
			return err
		}

		uploadDirectory(client, rpmCmdOpts.Bucket, rpmCmdOpts.Prefix, mergedRepoPath, rpmCmdOpts.Visibility)
	} else {
		logrus.Info("No existing repodata found in S3. Uploading new RPMs and repodata.")

		if rpmCmdOpts.Sign {
			logrus.Info("Signing new repository metadata.")
			if err := signRepo(rpmCmdOpts.SignPass, newRepoPath); err != nil {
				return err
			}
		}

		uploadDirectory(client, rpmCmdOpts.Bucket, rpmCmdOpts.Prefix, newRepoPath, rpmCmdOpts.Visibility)
	}

	return nil
}

func main() {
	cmd := &cobra.Command{
		Use:     "rpm",
		Short:   "Handle rpms in a S3 bucket",
		Long:    "The rpm is required to run in a OS/container with createrepo_c and mergerepo_c",
		RunE:    rpmTool,
		Version: version,
	}

	cmd.Flags().StringVarP(&rpmCmdOpts.Bucket, "bucket", "b", "", "S3 bucket")
	cmd.Flags().StringVarP(&rpmCmdOpts.Prefix, "prefix", "p", "", "S3 prefix")
	cmd.Flags().StringVar(&rpmCmdOpts.Visibility, "visibility", "private", "S3 ACL (default: \"private\")")
	cmd.Flags().StringVar(&rpmCmdOpts.AwsAccessKey, "aws-access-key", "", "AWS Access Key ID")
	cmd.Flags().StringVar(&rpmCmdOpts.AwsSecretKey, "aws-secret-key", "", "AWS Secret Access Key")
	cmd.Flags().BoolVar(&rpmCmdOpts.Sign, "sign", false, "Sign RPMs with rpmsign")
	cmd.Flags().StringVar(&rpmCmdOpts.SignPass, "sign-pass", "", "Passphrase for signing (can be empty)")
	cmd.Flags().BoolVar(&rpmCmdOpts.Rebuild, "rebuild", false, "Rebuild the repository metadata")

	if err := cmd.MarkFlagRequired("bucket"); err != nil {
		logrus.Fatal(err)
	}

	if err := cmd.MarkFlagRequired("aws-access-key"); err != nil {
		logrus.Fatal(err)
	}

	if err := cmd.MarkFlagRequired("aws-secret-key"); err != nil {
		logrus.Fatal(err)
	}

	if err := cmd.Execute(); err != nil {
		logrus.Fatal(err)
	}
}
