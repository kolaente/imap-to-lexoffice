package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
)

type Config struct {
	IMAPServer   string
	IMAPPort     string
	IMAPUser     string
	IMAPPassword string
	LexofficeKey string
	PollInterval time.Duration
}

// IgnorePatterns contains regex patterns for files to ignore during upload
var IgnorePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^AGB_`),
	regexp.MustCompile(`.ics$`),
	regexp.MustCompile(`^Receipt-`),
	// Add more patterns here as needed
}

func loadConfig() *Config {
	interval := 5 * time.Minute
	if val := os.Getenv("POLL_INTERVAL_MINUTES"); val != "" {
		if mins, err := time.ParseDuration(val + "m"); err == nil {
			interval = mins
		}
	}

	return &Config{
		IMAPServer:   os.Getenv("IMAP_SERVER"),
		IMAPPort:     getEnvOrDefault("IMAP_PORT", "993"),
		IMAPUser:     os.Getenv("IMAP_USER"),
		IMAPPassword: os.Getenv("IMAP_PASSWORD"),
		LexofficeKey: os.Getenv("LEXOFFICE_API_KEY"),
		PollInterval: interval,
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

func main() {
	runOnce := flag.Bool("once", false, "Run once and exit")
	flag.Parse()

	config := loadConfig()

	if config.IMAPServer == "" || config.IMAPUser == "" || config.IMAPPassword == "" || config.LexofficeKey == "" {
		log.Fatal("Missing required environment variables: IMAP_SERVER, IMAP_USER, IMAP_PASSWORD, LEXOFFICE_API_KEY")
	}

	if *runOnce {
		log.Println("Running once and exiting...")
		processMailbox(config)
		return
	}

	log.Printf("Starting mail processor. Polling every %v", config.PollInterval)

	ticker := time.NewTicker(config.PollInterval)
	defer ticker.Stop()

	// Process immediately on startup
	processMailbox(config)

	// Then process on ticker
	for range ticker.C {
		processMailbox(config)
	}
}

func processMailbox(config *Config) {
	log.Println("Connecting to IMAP server...")

	c, err := imapclient.DialTLS(config.IMAPServer+":"+config.IMAPPort, nil)
	if err != nil {
		log.Printf("Failed to connect: %v", err)
		return
	}
	defer c.Close()

	if err := c.Login(config.IMAPUser, config.IMAPPassword).Wait(); err != nil {
		log.Printf("Login failed: %v", err)
		return
	}

	log.Println("Logged in successfully")

	// Select INBOX
	mailbox, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		log.Printf("Failed to select INBOX: %v", err)
		return
	}

	if mailbox.NumMessages == 0 {
		log.Println("No messages in INBOX")
		return
	}

	log.Printf("Found %d messages in INBOX", mailbox.NumMessages)

	// Fetch all messages
	seqSet := imap.SeqSet{}
	seqSet.AddRange(1, mailbox.NumMessages)

	fetchOptions := &imap.FetchOptions{
		UID: true,
	}

	msgs, err := c.Fetch(seqSet, fetchOptions).Collect()
	if err != nil {
		log.Printf("Failed to fetch messages: %v", err)
		return
	}

	log.Printf("Processing %d messages from INBOX", len(msgs))

	// Process each message
	for _, msg := range msgs {
		if err := processMessage(c, msg.UID, config); err != nil {
			log.Printf("Failed to process message %d: %v", msg.UID, err)
		}
	}

	// Logout
	if err := c.Logout().Wait(); err != nil {
		log.Printf("Logout failed: %v", err)
	}
}

func processMessage(c *imapclient.Client, uid imap.UID, config *Config) error {
	log.Printf("Processing message UID %d", uid)

	fetchOptions := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{}},
	}

	seqSet := imap.UIDSet{}
	seqSet.AddNum(uid)

	msgs, err := c.Fetch(seqSet, fetchOptions).Collect()
	if err != nil {
		return fmt.Errorf("fetch failed: %w", err)
	}

	if len(msgs) == 0 {
		return fmt.Errorf("message not found")
	}

	msg := msgs[0]

	// Get the message body
	var bodyReader io.Reader
	for _, literal := range msg.BodySection {
		bodyReader = bytes.NewReader(literal.Bytes)
		break
	}

	if bodyReader == nil {
		return fmt.Errorf("no body found")
	}

	// Parse the email
	mr, err := mail.CreateReader(bodyReader)
	if err != nil {
		return fmt.Errorf("failed to create mail reader: %w", err)
	}

	hasAttachments := false

	// Process each part
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read part: %w", err)
		}

		switch h := part.Header.(type) {
		case *mail.AttachmentHeader:
			hasAttachments = true
			filename, _ := h.Filename()
			log.Printf("  Found attachment: %s", filename)

			// Check if file should be ignored
			if shouldIgnoreFile(filename) {
				log.Printf("  Skipping %s (matches ignore pattern)", filename)
				continue
			}

			// Read attachment data
			data, err := io.ReadAll(part.Body)
			if err != nil {
				log.Printf("  Failed to read attachment: %v", err)
				continue
			}

			// Upload to Lexoffice
			if err := uploadToLexoffice(filename, data, config); err != nil {
				log.Printf("  Failed to upload to Lexoffice: %v", err)
			} else {
				log.Printf("  Successfully uploaded %s to Lexoffice", filename)
			}
		}
	}

	if hasAttachments {
		// Move message to "done" folder
		if err := moveToFolder(c, uid, "done"); err != nil {
			return fmt.Errorf("failed to move message: %w", err)
		}
		log.Printf("Moved message %d to 'done' folder", uid)
	} else {
		log.Printf("Message %d has no attachments, skipping", uid)
	}

	return nil
}

func shouldIgnoreFile(filename string) bool {
	for _, pattern := range IgnorePatterns {
		if pattern.MatchString(filename) {
			return true
		}
	}
	return false
}

func uploadToLexoffice(filename string, data []byte, config *Config) error {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add file field
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}

	// Add type field
	if err := writer.WriteField("type", "voucher"); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "https://api.lexoffice.io/v1/files", body)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+config.LexofficeKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func moveToFolder(c *imapclient.Client, uid imap.UID, folderName string) error {
	// Ensure the folder exists
	if err := ensureFolderExists(c, folderName); err != nil {
		return err
	}

	seqSet := imap.UIDSet{}
	seqSet.AddNum(uid)

	// Copy to done folder
	copyCmd := c.Copy(seqSet, folderName)
	if _, err := copyCmd.Wait(); err != nil {
		return fmt.Errorf("copy failed: %w", err)
	}

	// Mark as deleted
	storeFlags := imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Flags:  []imap.Flag{imap.FlagDeleted},
		Silent: true,
	}

	if err := c.Store(seqSet, &storeFlags, nil).Close(); err != nil {
		return fmt.Errorf("store flags failed: %w", err)
	}

	// Expunge to permanently delete
	if err := c.Expunge().Close(); err != nil {
		return fmt.Errorf("expunge failed: %w", err)
	}

	return nil
}

func ensureFolderExists(c *imapclient.Client, folderName string) error {
	listCmd := c.List("", folderName, nil)
	mailboxes, err := listCmd.Collect()
	if err != nil {
		return fmt.Errorf("list failed: %w", err)
	}

	// Check if folder exists
	for _, mbox := range mailboxes {
		if strings.EqualFold(mbox.Mailbox, folderName) {
			return nil
		}
	}

	// Create folder if it doesn't exist
	log.Printf("Creating folder '%s'", folderName)
	if err := c.Create(folderName, nil).Wait(); err != nil {
		return fmt.Errorf("create folder failed: %w", err)
	}

	return nil
}
