package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

const (
	projectName = "boring-registry"
	envPrefix   = "BORING_REGISTRY"
)

const (
	logKeyCaller    = "caller"
	logKeyHostname  = "hostname"
	logKeyTimestamp = "timestamp"
)

var (
	flagJSON  bool
	flagDebug bool

	// S3 options.
	flagS3Bucket          string
	flagS3Prefix          string
	flagS3Region          string
	flagS3Endpoint        string
	flagS3PathStyle       bool
	flagS3SignedURLExpiry time.Duration

	// GCS options.
	flagGCSBucket          string
	flagGCSPrefix          string
	flagGCSServiceAccount  string
	flagGCSSignedURLExpiry time.Duration
)

var (
	logger log.Logger
)

var rootCmd = &cobra.Command{
	Use:           projectName,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := initializeConfig(cmd); err != nil {
			return err
		}

		logger = setupLogger(os.Stdout)

		if flagDebug {
			_ = level.Debug(logger).Log("msg", "debug mode enabled")
		}

		return nil
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "Enable json logging")
	rootCmd.PersistentFlags().BoolVar(&flagDebug, "debug", false, "Enable debug logging")
	rootCmd.PersistentFlags().StringVar(&flagS3Bucket, "storage-s3-bucket", "", "S3 bucket to use for the registry")
	rootCmd.PersistentFlags().StringVar(&flagS3Prefix, "storage-s3-prefix", "", "S3 bucket prefix to use for the registry")
	rootCmd.PersistentFlags().StringVar(&flagS3Region, "storage-s3-region", "", "S3 bucket region to use for the registry")
	rootCmd.PersistentFlags().StringVar(&flagS3Endpoint, "storage-s3-endpoint", "", "S3 bucket endpoint URL (required for MINIO)")
	rootCmd.PersistentFlags().BoolVar(&flagS3PathStyle, "storage-s3-pathstyle", false, "S3 use PathStyle (required for MINIO)")
	rootCmd.PersistentFlags().DurationVar(&flagS3SignedURLExpiry, "storage-s3-signedurl-expiry", 30*time.Second, "Generate S3 signed URL valid for X seconds. Only meaningful if used in combination with --storage-s3-signedurl")
	rootCmd.PersistentFlags().StringVar(&flagGCSBucket, "storage-gcs-bucket", "", "Bucket to use when using the GCS registry type")
	rootCmd.PersistentFlags().StringVar(&flagGCSPrefix, "storage-gcs-prefix", "", "Prefix to use when using the GCS registry type")
	rootCmd.PersistentFlags().StringVar(&flagGCSServiceAccount, "storage-gcs-sa-email", "", `Google service account email to be used for Application Default Credentials (ADC).
GOOGLE_APPLICATION_CREDENTIALS environment variable might be used as alternative.
For GCS presigned URLs this SA needs the iam.serviceAccountTokenCreator role.`)
	rootCmd.PersistentFlags().DurationVar(&flagGCSSignedURLExpiry, "storage-gcs-signedurl-expiry", 30*time.Second, "Generate GCS signed URL valid for X seconds. Only meaningful if used in combination with --gcs-signedurl")
}

func initializeConfig(cmd *cobra.Command) error {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.AutomaticEnv()
	bindFlags(cmd, v)
	return nil
}

func setupLogger(w io.Writer) log.Logger {
	logger := log.NewLogfmtLogger(w)

	if flagJSON {
		logger = log.NewJSONLogger(w)
	}

	logger = log.With(logger,
		logKeyCaller, log.Caller(5),
		logKeyTimestamp, log.DefaultTimestampUTC,
	)

	logLevel := level.AllowInfo()
	{
		if flagDebug {
			logLevel = level.AllowDebug()
		}
		logger = level.NewFilter(logger, logLevel)
	}

	if hostname, err := os.Hostname(); err == nil {
		logger = log.With(logger, logKeyHostname, hostname)
	}

	return logger
}

func bindFlags(cmd *cobra.Command, v *viper.Viper) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		envVarSuffix := strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if err := v.BindEnv(f.Name, fmt.Sprintf("%s_%s", envPrefix, envVarSuffix)); err != nil {
			panic(fmt.Errorf("failed to bind key to environment variable: %w", err))
		}
		if !f.Changed && v.IsSet(f.Name) {
			val := v.Get(f.Name)
			if err := cmd.Flags().Set(f.Name, fmt.Sprintf("%v", val)); err != nil {
				panic(fmt.Errorf("failed to set value of flag: %w", err))
			}
		}
	})
}
