package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"time"

	"github.com/TierMobility/boring-registry/pkg/core"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	s3manager "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-kit/kit/log"
	"github.com/pkg/errors"
)

// s3ClientAPI is used to mock the AWS APIs
// See https://aws.github.io/aws-sdk-go-v2/docs/unit-testing/
type s3ClientAPI interface {
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input, f ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	CopyObject(ctx context.Context, params *s3.CopyObjectInput, optFns ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
}

// s3UploaderAPI is used to mock the AWS APIs
// See https://aws.github.io/aws-sdk-go-v2/docs/unit-testing/
type s3UploaderAPI interface {
	Upload(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3manager.Uploader)) (*s3manager.UploadOutput, error)
}

// s3DownloaderAPI is used to mock the AWS APIs
// See https://aws.github.io/aws-sdk-go-v2/docs/unit-testing/
type s3DownloaderAPI interface {
	Download(ctx context.Context, w io.WriterAt, input *s3.GetObjectInput, options ...func(api *s3manager.Downloader)) (n int64, err error)
}

// S3Storage is a Storage implementation backed by S3.
// S3Storage implements module.Storage and provider.Storage
type S3Storage struct {
	client              s3ClientAPI
	presignClient       *s3.PresignClient
	downloader          s3DownloaderAPI
	uploader            s3UploaderAPI
	bucket              string
	bucketPrefix        string
	bucketRegion        string
	bucketEndpoint      string
	moduleArchiveFormat string
	forcePathStyle      bool
	signedURLExpiry     time.Duration
}

// GetModule retrieves information about a module from the S3 storage.
func (s *S3Storage) GetModule(ctx context.Context, namespace, name, provider, version string) (core.Module, error) {
	key := modulePath(s.bucketPrefix, namespace, name, provider, version, s.moduleArchiveFormat)

	exists, err := s.objectExists(ctx, key)
	if err != nil {
		return core.Module{}, err
	} else if !exists {
		return core.Module{}, ErrModuleNotFound
	}

	presigned, err := s.presignedURL(ctx, key)
	if err != nil {
		return core.Module{}, err
	}

	return core.Module{
		Namespace:   namespace,
		Name:        name,
		Provider:    provider,
		Version:     version,
		DownloadURL: presigned,
	}, nil
}

func (s *S3Storage) ListModuleVersions(ctx context.Context, namespace, name, provider string) ([]core.Module, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(modulePathPrefix(s.bucketPrefix, namespace, name, provider)),
	}

	var modules []core.Module
	paginator := s3.NewListObjectsV2Paginator(s.client, input)
	for paginator.HasMorePages() {
		resp, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, errors.Wrap(ErrModuleListFailed, err.Error())
		}

		for _, obj := range resp.Contents {
			m, err := moduleFromObject(*obj.Key, s.moduleArchiveFormat)
			if err != nil {
				// TODO: we're skipping possible failures silently
				continue
			}

			// The download URL is probably not necessary for ListModules
			m.DownloadURL, err = s.presignedURL(ctx, modulePath(s.bucketPrefix, m.Namespace, m.Name, m.Provider, m.Version, s.moduleArchiveFormat))
			if err != nil {
				return []core.Module{}, err
			}

			modules = append(modules, *m)
		}
	}

	return modules, nil
}

// UploadModule uploads a module to the S3 storage.
func (s *S3Storage) UploadModule(ctx context.Context, namespace, name, provider, version string, body io.Reader) (core.Module, error) {
	if namespace == "" {
		return core.Module{}, errors.New("namespace not defined")
	}

	if name == "" {
		return core.Module{}, errors.New("name not defined")
	}

	if provider == "" {
		return core.Module{}, errors.New("provider not defined")
	}

	if version == "" {
		return core.Module{}, errors.New("version not defined")
	}

	key := modulePath(s.bucketPrefix, namespace, name, provider, version, DefaultModuleArchiveFormat)

	if _, err := s.GetModule(ctx, namespace, name, provider, version); err == nil {
		return core.Module{}, errors.Wrap(ErrModuleAlreadyExists, key)
	}

	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   body,
	}

	if _, err := s.uploader.Upload(ctx, input); err != nil {
		return core.Module{}, errors.Wrapf(ErrModuleUploadFailed, err.Error())
	}

	return s.GetModule(ctx, namespace, name, provider, version)
}

// MigrateModules is only a temporary method needed for the migration from 0.7.0 to 0.8.0 and above
func (s *S3Storage) MigrateModules(ctx context.Context, logger log.Logger, dryRun bool) error {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(path.Join(s.bucketPrefix, string(internalModuleType))),
	}

	paginator := s3.NewListObjectsV2Paginator(s.client, input)
	for paginator.HasMorePages() {
		resp, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("failed to page: %w", err)
		}

		for _, obj := range resp.Contents {
			if !isUnmigratedModule(s.bucketPrefix, *obj.Key) {
				_ = logger.Log("message", "skipping...", "key", *obj.Key)
				continue
			}

			targetKey := aws.String(migrationTargetPath(s.bucketPrefix, s.moduleArchiveFormat, *obj.Key))
			if dryRun {
				_ = logger.Log("message", "skipping due to dry-run", "source", obj.Key, "target", *targetKey)
			} else {
				_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
					Bucket:     aws.String(s.bucket),
					CopySource: aws.String(url.PathEscape(path.Join(s.bucket, *obj.Key))),
					Key:        targetKey,
				})
				if err != nil {
					return err
				}

				_ = logger.Log("message", "copied module", "source", *obj.Key, "target", targetKey)
			}
		}
	}

	return nil
}

// MigrateProviders is a temporary method needed for the migration from 0.7.0 to 0.8.0 and above
func (s *S3Storage) MigrateProviders(ctx context.Context, logger log.Logger, dryRun bool) error {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(path.Join(s.bucketPrefix, string(internalProviderType))),
	}

	paginator := s3.NewListObjectsV2Paginator(s.client, input)
	for paginator.HasMorePages() {
		resp, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("failed to page: %w", err)
		}

		for _, obj := range resp.Contents {
			directory, err := providerMigrationTargetPath(s.bucketPrefix, *obj.Key)
			if err != nil {
				return err
			}

			targetKey := path.Join(directory, path.Base(*obj.Key))

			if dryRun {
				_ = logger.Log("message", "skipping due to dry-run", "source", obj.Key, "target", targetKey)
			} else {
				_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
					Bucket:     aws.String(s.bucket),
					CopySource: aws.String(url.PathEscape(path.Join(s.bucket, *obj.Key))),
					Key:        aws.String(targetKey),
				})
				if err != nil {
					return err
				}

				_ = logger.Log("message", "copied module", "source", *obj.Key, "target", targetKey)
			}
		}
	}

	return nil
}

// GetProvider retrieves information about a provider from the S3 storage.
func (s *S3Storage) GetProvider(ctx context.Context, namespace, name, version, os, arch string) (core.Provider, error) {
	archivePath, shasumPath, shasumSigPath, err := internalProviderPath(s.bucketPrefix, namespace, name, version, os, arch)
	if err != nil {
		return core.Provider{}, err
	}

	zipURL, err := s.presignedURL(ctx, archivePath)
	if err != nil {
		return core.Provider{}, err
	}
	shasumsURL, err := s.presignedURL(ctx, shasumPath)
	if err != nil {
		return core.Provider{}, errors.Wrap(err, shasumPath)
	}
	signatureURL, err := s.presignedURL(ctx, shasumSigPath)
	if err != nil {
		return core.Provider{}, err
	}

	shasumBytes, err := s.download(ctx, shasumPath)
	if err != nil {
		return core.Provider{}, err
	}

	shasum, err := readSHASums(bytes.NewReader(shasumBytes), path.Base(archivePath))
	if err != nil {
		return core.Provider{}, err
	}

	signingKeys, err := s.SigningKeys(ctx, namespace)
	if err != nil {
		return core.Provider{}, err
	}

	return core.Provider{
		Namespace:           namespace,
		Name:                name,
		Version:             version,
		OS:                  os,
		Arch:                arch,
		Shasum:              shasum,
		Filename:            path.Base(archivePath),
		DownloadURL:         zipURL,
		SHASumsURL:          shasumsURL,
		SHASumsSignatureURL: signatureURL,
		SigningKeys:         *signingKeys,
	}, nil
}

func (s *S3Storage) ListProviderVersions(ctx context.Context, namespace, name string) ([]core.ProviderVersion, error) {
	prefix, err := providerStoragePrefix(s.bucketPrefix, internalProviderType, "", namespace, name)
	if err != nil {
		return nil, err
	}

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fmt.Sprintf("%s/", prefix)),
	}

	collection := NewCollection()
	paginator := s3.NewListObjectsV2Paginator(s.client, input)
	for paginator.HasMorePages() {
		resp, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, errors.Wrap(ErrProviderListFailed, err.Error())
		}

		for _, obj := range resp.Contents {
			provider, err := core.NewProviderFromArchive(*obj.Key)
			if err != nil {
				continue
			}

			collection.Add(provider)
		}
	}

	result := collection.List()

	if len(result) == 0 {
		return nil, fmt.Errorf("no provider versions found for %s/%s", namespace, name)
	}

	return result, nil
}

func (s *S3Storage) UploadProviderReleaseFiles(ctx context.Context, namespace, name, filename string, file io.Reader) error {
	if namespace == "" {
		return fmt.Errorf("namespace argument is empty")
	}

	if name == "" {
		return fmt.Errorf("name argument is empty")
	}

	if filename == "" {
		return fmt.Errorf("name argument is empty")
	}

	prefix, err := providerStoragePrefix(s.bucketPrefix, internalProviderType, "", namespace, name)
	if err != nil {
		return err
	}

	key := filepath.Join(prefix, filename)
	exists, err := s.objectExists(ctx, key)
	if err != nil {
		return err
	} else if exists {
		return ErrProviderAlreadyExists
	}

	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   file,
	}

	if _, err = s.uploader.Upload(ctx, input); err != nil {
		return fmt.Errorf("failed to upload provider: %w", err)
	}

	return nil
}

// SigningKeys downloads the JSON placed in the namespace in S3 and unmarshals it into a core.SigningKeys
func (s *S3Storage) SigningKeys(ctx context.Context, namespace string) (*core.SigningKeys, error) {
	if namespace == "" {
		return nil, fmt.Errorf("namespace argument is empty")
	}

	signingKeysRaw, err := s.download(ctx, signingKeysPath(s.bucketPrefix, namespace))
	if err != nil {
		return nil, fmt.Errorf("failed to download signing_keys.json for namespace %s: %w", namespace, err)
	}

	return unmarshalSigningKeys(signingKeysRaw)
}

func (s *S3Storage) presignedURL(ctx context.Context, key string) (string, error) {
	presignResult, err := s.presignClient.PresignGetObject(ctx,
		&s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		},
		s3.WithPresignExpires(s.signedURLExpiry), // TODO(oliviermichaelis): check if we need to set it back to 15min
	)

	return presignResult.URL, err
}

func (s *S3Storage) objectExists(ctx context.Context, key string) (bool, error) {
	input := &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}

	if _, err := s.client.HeadObject(ctx, input); err != nil {
		var responseError *awshttp.ResponseError
		if errors.As(err, &responseError) && responseError.ResponseError.HTTPStatusCode() == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

func (s *S3Storage) download(ctx context.Context, path string) ([]byte, error) {
	buf := s3manager.NewWriteAtBuffer([]byte{})

	input := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	}

	if _, err := s.downloader.Download(ctx, buf, input); err != nil {
		return nil, errors.Wrapf(err, "failed to download: %s", path)
	}

	return buf.Bytes(), nil
}

// S3StorageOption provides additional options for the S3Storage.
type S3StorageOption func(*S3Storage)

// WithS3StorageBucketPrefix configures the s3 storage to work under a given prefix.
func WithS3StorageBucketPrefix(prefix string) S3StorageOption {
	return func(s *S3Storage) {
		s.bucketPrefix = prefix
	}
}

// WithS3StorageBucketRegion configures the region for a given s3 storage.
// TODO: the AWS signing region could be another one as the bucket location
func WithS3StorageBucketRegion(region string) S3StorageOption {
	return func(s *S3Storage) {
		s.bucketRegion = region
	}
}

// WithS3StorageBucketEndpoint configures the endpoint for a given s3 storage. (needed for MINIO)
func WithS3StorageBucketEndpoint(endpoint string) S3StorageOption {
	return func(s *S3Storage) {
		s.bucketEndpoint = endpoint
	}
}

// WithS3ArchiveFormat configures the module archive format (zip, tar, tgz, etc.)
func WithS3ArchiveFormat(archiveFormat string) S3StorageOption {
	return func(s *S3Storage) {
		s.moduleArchiveFormat = archiveFormat
	}
}

// WithS3StoragePathStyle configures if Path Style is used for a given s3 storage. (needed for MINIO)
func WithS3StoragePathStyle(forcePathStyle bool) S3StorageOption {
	return func(s *S3Storage) {
		s.forcePathStyle = forcePathStyle
	}
}

// WithS3StorageSignedUrlExpiry configures the duration until the signed url expires
func WithS3StorageSignedUrlExpiry(t time.Duration) S3StorageOption {
	return func(s *S3Storage) {
		s.signedURLExpiry = t
	}
}

// NewS3Storage returns a fully initialized S3 storage.
func NewS3Storage(ctx context.Context, bucket string, options ...S3StorageOption) (*S3Storage, error) {
	// Required- and default-values should be set here
	s := &S3Storage{
		bucket: bucket,
	}

	for _, option := range options {
		option(s)
	}

	// The EndpointResolver is used for compatibility with MinIO
	customResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		if s.bucketEndpoint != "" {
			return aws.Endpoint{
				PartitionID:       "aws",
				URL:               s.bucketEndpoint,
				HostnameImmutable: true, // Needs to be true for MinIO
			}, nil
		}

		// returning EndpointNotFoundError will allow the service to fall back to its default resolution
		return aws.Endpoint{}, &aws.EndpointNotFoundError{}
	})

	// Create the S3 client
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(s.bucketRegion), config.WithEndpointResolverWithOptions(customResolver))
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg)
	s.client = client
	s.presignClient = s3.NewPresignClient(client)
	s.uploader = s3manager.NewUploader(client)
	s.downloader = s3manager.NewDownloader(client)

	if s.bucketRegion == "" {
		region, err := s3manager.GetBucketRegion(ctx, client, s.bucket)
		if err != nil {
			return nil, errors.Wrap(err, "failed to determine bucket region")
		}
		s.bucketRegion = region
	}

	return s, nil
}
