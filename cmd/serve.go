/*
Copyright © 2023 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	garmin "github.com/abrander/garmin-connect"
	"github.com/dropbox/dropbox-sdk-go-unofficial/v6/dropbox"
	"github.com/dropbox/dropbox-sdk-go-unofficial/v6/dropbox/files"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

const (
	WahooAppsPath string = "/Apps/WahooFitness"
	processedTag  string = "warminprocessed"
)

var oauthConf *oauth2.Config

var errDuplicateActivity error = fmt.Errorf("Duplicate Activity.")
var errAccepted error = fmt.Errorf("202: Accepted")

var logger *zap.Logger

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "A brief description of your command",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(cmd.Context())
		logger, _ = zap.NewProduction()

		mux := http.NewServeMux()
		mux.HandleFunc("/callback", oauthCallbackHandler)

		server := &http.Server{
			Addr:    ":3000",
			Handler: mux,
		}

		dropboxClientID := viper.GetString("DropboxClientID")
		dropboxClientSecret := viper.GetString("DropboxClientSecret")

		oauthConf = &oauth2.Config{
			ClientID:     dropboxClientID,
			ClientSecret: dropboxClientSecret,
			Scopes:       []string{"files.metadata.write", "account_info.read", "files.content.read"},
			RedirectURL:  "http://localhost:3000/callback",
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://www.dropbox.com/oauth2/authorize",
				TokenURL: "https://api.dropboxapi.com/oauth2/token",
			},
		}

		go func() {
			sigint := make(chan os.Signal, 1)
			signal.Notify(sigint, os.Interrupt)
			<-sigint

			// We received an interrupt signal, shut down.
			if err := server.Shutdown(context.Background()); err != nil {
				// Error from closing listeners, or context timeout:
				logger.Warn("HTTP server Shutdown", zap.Error(err))
			}
			cancel()
		}()

		refreshToken := viper.GetString("refreshToken")
		if refreshToken == "" {
			// Redirect user to consent page to ask for permission
			// for the scopes specified above.
			url := oauthConf.AuthCodeURL("state", oauth2.SetAuthURLParam("token_access_type", "offline"))
			fmt.Printf("Visit the URL for the auth dialog: %v", url)
			if err := server.ListenAndServe(); err != http.ErrServerClosed {
				// Error starting or closing listener:
				logger.Error("HTTP server ListenAndServe", zap.Error(err))
			}

			<-ctx.Done()
			return
		}

		// we have a refreshToken so use it
		token := &oauth2.Token{RefreshToken: refreshToken}
		go syncWahoo(ctx, token)
		<-ctx.Done()
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// serveCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// serveCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}

func oauthCallbackHandler(w http.ResponseWriter, r *http.Request) {

	code := r.URL.Query().Get("code")
	if code == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	logger.Info("got oauth2 callback", zap.String("code", code))
	token, err := oauthConf.Exchange(context.TODO(), code)
	if err != nil {
		log.Fatal(err)
	}

	if token.Valid() {
		viper.Set("refreshToken", token.RefreshToken)
		logger.Info("saving refreshToken")
		err := viper.WriteConfig()
		if err != nil {
			logger.Error("cannot persist refresh token", zap.Error(err))
		}
		go syncWahoo(context.Background(), token)

		logger.Info("valid token", zap.String("accessToken", token.AccessToken), zap.String("refreshTOken", token.RefreshToken))
	}

	w.WriteHeader(http.StatusOK)
}

// syncWahoo syncs wahoo resources from dropbox to garmin
// TODO(daa): call in loop and refresh token
func syncWahoo(ctx context.Context, token *oauth2.Token) {
	logger.Info("starting wahoo sync", zap.Time("tokenExpiry", token.Expiry), zap.String("accessToken", token.AccessToken), zap.String("refreshToken", token.RefreshToken))
	var err error

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	email := viper.GetString("GarminEmail")
	password := viper.GetString("GarminPassword")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(5 * time.Minute)
			if !token.Valid() {
				logger.Info("token is not valid anymore, refreshing token")
				ts := oauthConf.TokenSource(ctx, token)
				token, err = ts.Token()
				if err != nil {
					logger.Error("cannot refresh token", zap.Error(err))
				}
				viper.Set("refreshToken", token.RefreshToken)
				err := viper.WriteConfig()
				if err != nil {
					logger.Error("cannot persist refresh token", zap.Error(err))
				}
			}

			config := dropbox.Config{
				Token:    token.AccessToken,
				LogLevel: dropbox.LogInfo, // if needed, set the desired logging level. Default is off
			}

			dbx := files.New(config)
			// start making API calls

			wahooFiles, err := listFiles(dbx, logger)
			if err != nil {
				logger.Panic("error listing files", zap.Error(err))
			}

			gClient := garmin.NewClient(garmin.AutoRenewSession(true), garmin.Credentials(email, password))

			for _, f := range wahooFiles {
				logger.Debug("found wahoo file", zap.String("name", f.Name), zap.String("path", f.PathLower), zap.Uint64("size", f.Size))

				if isFileProcessed(dbx, f) {
					logger.Debug("skipping already processed file", zap.String("name", f.Name))
					continue
				}

				_, c, err := dbx.Download(&files.DownloadArg{
					Path: f.PathLower,
				})
				if err != nil {
					logger.Panic("error downloading file", zap.String("name", f.Name), zap.Error(err))
				}

				added, err := gClient.ImportActivity(c, garmin.ActivityFormatFIT)
				if err != nil && err.Error() != errDuplicateActivity.Error() && err.Error() != errAccepted.Error() {
					logger.Panic("cannot upload data to garmin", zap.String("name", f.Name), zap.Error(err))
				}

				logger.Info("Imported new activity to garmin", zap.Int("garminActivity", added), zap.String("name", f.Name))

				err = dbx.TagsAdd(&files.AddTagArg{Path: f.PathLower, TagText: processedTag})
				if err != nil {
					logger.Panic("cannot tag file as processed", zap.String("name", f.Name), zap.Error(err))
				}
				logger.Info("Marked file as processed", zap.String("name", f.Name))
			}
		}
	}

}

func isFileProcessed(dbx files.Client, f files.FileMetadata) bool {
	tagsResponse, err := dbx.TagsGet(&files.GetTagsArg{
		Paths: []string{f.PathLower},
	})
	if err != nil {
		return false
	}

	for _, tagPath := range tagsResponse.PathsToTags {
		if tagPath.Path != f.PathLower {
			continue
		}
		for _, t := range tagPath.Tags {
			if t == nil {
				continue
			}
			if t.UserGeneratedTag.TagText == processedTag {
				return true
			}
		}
	}

	return false
}

func listFiles(dbx files.Client, logger *zap.Logger) ([]files.FileMetadata, error) {
	res, err := dbx.ListFolder(&files.ListFolderArg{
		Path:           WahooAppsPath,
		Recursive:      false,
		IncludeDeleted: false,
	})
	if err != nil {

		logger.Panic("error reading Wahoo Folder", zap.Error(err))
	}

	fs := []files.FileMetadata{}
	for _, entry := range res.Entries {
		if conv, ok := entry.(*files.FileMetadata); ok {
			fs = append(fs, *conv)
		}
	}
	for res.HasMore {
		res, err = dbx.ListFolderContinue(&files.ListFolderContinueArg{Cursor: res.Cursor})
		if err != nil {
			return fs, err
		}
		for _, entry := range res.Entries {
			if conv, ok := entry.(*files.FileMetadata); ok {
				fs = append(fs, *conv)
			}
		}
	}

	return fs, nil
}
