package pg

import (
	"fmt"

	"github.com/wal-g/wal-g/utility"

	"github.com/pkg/errors"
	"github.com/wal-g/wal-g/internal/databases/postgres"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
)

const (
	backupPushShortDescription = "Makes backup and uploads it to storage"

	permanentFlag             = "permanent"
	fullBackupFlag            = "full"
	verifyPagesFlag           = "verify"
	storeAllCorruptBlocksFlag = "store-all-corrupt"
	useRatingComposerFlag     = "rating-composer"
	useCopyComposerFlag       = "copy-composer"
	deltaFromUserDataFlag     = "delta-from-user-data"
	deltaFromNameFlag         = "delta-from-name"
	addUserDataFlag           = "add-user-data"
	withoutFilesMetadataFlag  = "without-files-metadata"

	permanentShorthand             = "p"
	fullBackupShorthand            = "f"
	verifyPagesShorthand           = "v"
	storeAllCorruptBlocksShorthand = "s"
	useRatingComposerShorthand     = "r"
	useCopyComposerShorthand       = "c"
)

var (
	// backupPushCmd represents the backupPush command
	backupPushCmd = &cobra.Command{
		Use:   "backup-push db_directory",
		Short: backupPushShortDescription, // TODO : improve description
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			var dataDirectory string

			if len(args) > 0 {
				dataDirectory = args[0]
			}

			verifyPageChecksums = verifyPageChecksums || viper.GetBool(internal.VerifyPageChecksumsSetting)
			storeAllCorruptBlocks = storeAllCorruptBlocks || viper.GetBool(internal.StoreAllCorruptBlocksSetting)
			tarBallComposerType := postgres.RegularComposer

			useRatingComposer = useRatingComposer || viper.GetBool(internal.UseRatingComposerSetting)
			if useRatingComposer {
				tarBallComposerType = postgres.RatingComposer
			}
			useCopyComposer = useCopyComposer || viper.GetBool(internal.UseCopyComposerSetting)
			if useCopyComposer {
				fullBackup = true
				tarBallComposerType = postgres.CopyComposer
			}
			if deltaFromName == "" {
				deltaFromName = viper.GetString(internal.DeltaFromNameSetting)
			}
			if deltaFromUserData == "" {
				deltaFromUserData = viper.GetString(internal.DeltaFromUserDataSetting)
			}
			if userDataRaw == "" {
				userDataRaw = viper.GetString(internal.SentinelUserDataSetting)
			}
			withoutFilesMetadata = withoutFilesMetadata || viper.GetBool(internal.WithoutFilesMetadataSetting)
			if withoutFilesMetadata {
				// files metadata tracking is required for delta backups and copy/rating composers
				if useRatingComposer || useCopyComposer {
					tracelog.ErrorLogger.Fatalf(
						"%s option cannot be used with %s, %s options",
						withoutFilesMetadataFlag, useRatingComposerFlag, useCopyComposerFlag)
				}
				if deltaFromName != "" || deltaFromUserData != "" || userDataRaw != "" {
					tracelog.ErrorLogger.Fatalf(
						"%s option cannot be used with %s, %s, %s options",
						withoutFilesMetadataFlag, deltaFromNameFlag, deltaFromUserDataFlag, addUserDataFlag)
				}
				tracelog.InfoLogger.Print("Files metadata tracking is disabled")
				fullBackup = true
			}

			deltaBaseSelector, err := createDeltaBaseSelector(cmd, deltaFromName, deltaFromUserData)
			tracelog.ErrorLogger.FatalOnError(err)

			userData, err := internal.UnmarshalSentinelUserData(userDataRaw)
			tracelog.ErrorLogger.FatalfOnError("Failed to unmarshal the provided UserData: %s", err)

			arguments := postgres.NewBackupArguments(dataDirectory, utility.BaseBackupPath,
				permanent, verifyPageChecksums || viper.GetBool(internal.VerifyPageChecksumsSetting),
				fullBackup, storeAllCorruptBlocks || viper.GetBool(internal.StoreAllCorruptBlocksSetting),
				tarBallComposerType, deltaBaseSelector, userData, withoutFilesMetadata)

			backupHandler, err := postgres.NewBackupHandler(arguments)
			tracelog.ErrorLogger.FatalOnError(err)
			backupHandler.HandleBackupPush()
		},
	}
	permanent             = false
	fullBackup            = false
	verifyPageChecksums   = false
	storeAllCorruptBlocks = false
	useRatingComposer     = false
	useCopyComposer       = false
	deltaFromName         = ""
	deltaFromUserData     = ""
	userDataRaw           = ""
	withoutFilesMetadata  = false
)

// create the BackupSelector for delta backup base according to the provided flags
func createDeltaBaseSelector(cmd *cobra.Command,
	targetBackupName, targetUserData string) (internal.BackupSelector, error) {
	switch {
	case targetUserData != "" && targetBackupName != "":
		fmt.Println(cmd.UsageString())
		return nil, errors.New("only one delta target should be specified")

	case targetBackupName != "":
		tracelog.InfoLogger.Printf("Selecting the backup with name %s as the base for the current delta backup...\n",
			targetBackupName)
		return internal.NewBackupNameSelector(targetBackupName, true)

	case targetUserData != "":
		tracelog.InfoLogger.Println(
			"Selecting the backup with specified user data as the base for the current delta backup...")
		return internal.NewUserDataBackupSelector(targetUserData, postgres.NewGenericMetaFetcher())

	default:
		return internal.NewLatestBackupSelector(), nil
	}
}

func init() {
	Cmd.AddCommand(backupPushCmd)

	backupPushCmd.Flags().BoolVarP(&permanent, permanentFlag, permanentShorthand,
		false, "Pushes permanent backup")
	backupPushCmd.Flags().BoolVarP(&fullBackup, fullBackupFlag, fullBackupShorthand,
		false, "Make full backup-push")
	backupPushCmd.Flags().BoolVarP(&verifyPageChecksums, verifyPagesFlag, verifyPagesShorthand,
		false, "Verify page checksums")
	backupPushCmd.Flags().BoolVarP(&storeAllCorruptBlocks, storeAllCorruptBlocksFlag, storeAllCorruptBlocksShorthand,
		false, "Store all corrupt blocks found during page checksum verification")
	backupPushCmd.Flags().BoolVarP(&useRatingComposer, useRatingComposerFlag, useRatingComposerShorthand,
		false, "Use rating tar composer (beta)")
	backupPushCmd.Flags().BoolVarP(&useCopyComposer, useCopyComposerFlag, useCopyComposerShorthand,
		false, "Use copy tar composer (beta)")
	backupPushCmd.Flags().StringVar(&deltaFromName, deltaFromNameFlag,
		"", "Select the backup specified by name as the target for the delta backup")
	backupPushCmd.Flags().StringVar(&deltaFromUserData, deltaFromUserDataFlag,
		"", "Select the backup specified by UserData as the target for the delta backup")
	backupPushCmd.Flags().StringVar(&userDataRaw, addUserDataFlag,
		"", "Write the provided user data to the backup sentinel and metadata files.")
	backupPushCmd.Flags().BoolVar(&withoutFilesMetadata, withoutFilesMetadataFlag,
		false, "Do not track files metadata, significantly reducing memory usage")
}
