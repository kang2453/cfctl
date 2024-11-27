package other

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/eiannone/keyboard"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/jhump/protoreflect/dynamic"

	"github.com/jhump/protoreflect/grpcreflect"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

//const encryptionKey = "spaceone-cfctl-encryption-key-32byte"

const (
	keyringService = "cfctl-credentials"
	keyringUser    = "encryption-key"
)

var providedUrl string

// LoginCmd represents the login command
var LoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Login to SpaceONE",
	Long: `A command that allows you to login to SpaceONE.
It will prompt you for your User ID, Password, and fetch the Domain ID automatically, then fetch the token.`,
	Run: executeLogin,
}

// tokenAuth implements grpc.PerRPCCredentials for token-based authentication.
type tokenAuth struct {
	token string
}

func (t *tokenAuth) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"token": t.token, // "Authorization: Bearer" 대신 "token" 키 사용
	}, nil
}

func (t *tokenAuth) RequireTransportSecurity() bool {
	return true
}

func executeLogin(cmd *cobra.Command, args []string) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		pterm.Error.Println("Failed to get user home directory:", err)
		return
	}

	configPath := filepath.Join(homeDir, ".cfctl", "config.yaml")
	viper.SetConfigFile(configPath)

	if err := viper.ReadInConfig(); err != nil {
		pterm.Error.Printf("Failed to read config file: %v\n", err)
		return
	}

	currentEnv := viper.GetString("environment")
	if currentEnv == "" {
		pterm.Error.Println("No environment selected")
		return
	}

	// Check if it's an app environment
	if strings.HasSuffix(currentEnv, "-app") {
		if err := executeAppLogin(currentEnv); err != nil {
			pterm.Error.Printf("Login failed: %v\n", err)
			return
		}
	} else {
		// Execute normal user login
		executeUserLogin(currentEnv)
	}
}

type TokenInfo struct {
	Token string `yaml:"token"`
}

// promptToken prompts for token input
func promptToken() (string, error) {
	prompt := &survey.Password{
		Message: "Enter your token:",
	}

	var token string
	err := survey.AskOne(prompt, &token, survey.WithValidator(survey.Required))
	if err != nil {
		return "", err
	}

	return token, nil
}

// saveAppToken saves the token
func saveAppToken(currentEnv, token string) error {
	homeDir, _ := os.UserHomeDir()
	configPath := filepath.Join(homeDir, ".cfctl", "config.yaml")

	viper.SetConfigFile(configPath)
	if err := viper.ReadInConfig(); err != nil && !os.IsNotExist(err) {
		return err
	}

	envPath := fmt.Sprintf("environments.%s", currentEnv)
	envSettings := viper.GetStringMap(envPath)
	if envSettings == nil {
		envSettings = make(map[string]interface{})
	}

	// Initialize tokens array if it doesn't exist
	var tokens []TokenInfo
	if existingTokens, ok := envSettings["tokens"]; ok {
		if tokenList, ok := existingTokens.([]interface{}); ok {
			for _, t := range tokenList {
				if tokenMap, ok := t.(map[string]interface{}); ok {
					tokenInfo := TokenInfo{
						Token: tokenMap["token"].(string),
					}
					tokens = append(tokens, tokenInfo)
				}
			}
		}
	}

	// Add new token if it doesn't exist
	tokenExists := false
	for _, t := range tokens {
		if t.Token == token {
			tokenExists = true
			break
		}
	}

	if !tokenExists {
		newToken := TokenInfo{
			Token: token,
		}
		tokens = append(tokens, newToken)
	}

	// Update environment settings
	envSettings["tokens"] = tokens

	// Keep the existing endpoint and proxy settings
	if endpoint, ok := envSettings["endpoint"]; ok {
		envSettings["endpoint"] = endpoint
	}
	if proxy, ok := envSettings["proxy"]; ok {
		envSettings["proxy"] = proxy
	}

	viper.Set(envPath, envSettings)
	return viper.WriteConfig()
}

// promptTokenSelection shows available tokens and lets user select one
func promptTokenSelection(tokens []TokenInfo) (string, error) {
	if len(tokens) == 0 {
		return "", fmt.Errorf("no tokens available")
	}

	if err := keyboard.Open(); err != nil {
		return "", err
	}
	defer keyboard.Close()

	selectedIndex := 0
	for {
		fmt.Print("\033[H\033[2J") // Clear screen

		pterm.DefaultHeader.WithFullWidth().
			WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
			WithTextStyle(pterm.NewStyle(pterm.FgLightWhite)).
			Println("Select a token:")

		// Display available tokens
		for i, token := range tokens {
			maskedToken := maskToken(token.Token)
			if i == selectedIndex {
				pterm.Printf("→ %d: %s\n", i+1, maskedToken)
			} else {
				pterm.Printf("  %d: %s\n", i+1, maskedToken)
			}
		}

		pterm.DefaultBasicText.WithStyle(pterm.NewStyle(pterm.FgGray)).
			Println("\nNavigation: [j]down [k]up [Enter]select [q]quit")

		char, key, err := keyboard.GetKey()
		if err != nil {
			return "", err
		}

		switch key {
		case keyboard.KeyEnter:
			return tokens[selectedIndex].Token, nil
		}

		switch char {
		case 'j':
			if selectedIndex < len(tokens)-1 {
				selectedIndex++
			}
		case 'k':
			if selectedIndex > 0 {
				selectedIndex--
			}
		case 'q', 'Q':
			return "", fmt.Errorf("selection cancelled")
		}
	}
}

// maskToken returns a masked version of the token for display
func maskToken(token string) string {
	if len(token) <= 10 {
		return strings.Repeat("*", len(token))
	}
	return token[:5] + "..." + token[len(token)-5:]
}

// executeAppLogin handles login for app environments
func executeAppLogin(currentEnv string) error {
	// Get token from user
	token, err := promptToken()
	if err != nil {
		return err
	}

	// Save token to tokens array
	if err := saveAppToken(currentEnv, token); err != nil {
		return err
	}

	pterm.Success.Printf("Token successfully saved\n")
	return nil
}

func executeUserLogin(currentEnv string) {
	loadEnvironmentConfig() // Load the environment-specific configuration

	baseUrl := providedUrl
	if baseUrl == "" {
		pterm.Error.Println("No token endpoint specified in the configuration file.")
		exitWithError()
	}

	// Get the home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		pterm.Error.Println("Failed to get user home directory:", err)
		exitWithError()
	}

	cacheViper := viper.New()
	cacheConfigPath := filepath.Join(homeDir, ".cfctl", "cache", "config.yaml")
	cacheViper.SetConfigFile(cacheConfigPath)

	var userID string
	var password string

	if err := cacheViper.ReadInConfig(); err == nil {
		usersField := cacheViper.Get("environments." + currentEnv + ".users")
		if usersField != nil {
			users, ok := usersField.([]interface{})
			if !ok {
				pterm.Error.Println("Failed to load users correctly.")
				exitWithError()
			}

			if len(users) > 0 {
				pterm.Info.Println("Select an account to login or add a new user:")

				// Display user selection including "Add new user" option
				userSelection := promptUserSelection(len(users), users)

				if userSelection <= len(users) {
					// Selected existing user
					selectedUser := users[userSelection-1].(map[string]interface{})
					userID = selectedUser["userid"].(string)
					encryptedPassword := selectedUser["password"].(string)
					token := selectedUser["token"].(string)

					// Check if token is still valid
					if !isTokenExpired(token) {
						// Use stored password
						decryptedPassword, err := decrypt(encryptedPassword)
						if err != nil {
							pterm.Error.Printf("Failed to decrypt password: %v\n", err)
							exitWithError()
						}
						password = decryptedPassword
						pterm.Success.Printf("Using saved credentials for %s\n", userID)
					} else {
						// Token expired, ask for password again
						password = promptPassword()
						// Verify the password matches
						decryptedPassword, err := decrypt(encryptedPassword)
						if err != nil {
							pterm.Error.Printf("Failed to decrypt password: %v\n", err)
							exitWithError()
						}
						if password != decryptedPassword {
							pterm.Error.Println("Password does not match.")
							exitWithError()
						}
					}
				} else {
					// Selected to add new user
					userID, password = promptCredentials()
				}
			} else {
				// No existing users, prompt for new credentials
				userID, password = promptCredentials()
			}
		} else {
			// Users field doesn't exist, prompt for new credentials
			userID, password = promptCredentials()
		}
	} else {
		// Configuration cannot be read, prompt for new credentials
		userID, password = promptCredentials()
	}

	// Proceed with domain ID fetching and token issuance
	mainViper := viper.New()
	mainViper.SetConfigFile(filepath.Join(homeDir, ".cfctl", "config.yaml"))
	if err := mainViper.ReadInConfig(); err != nil {
		pterm.Error.Println("Failed to read main config file:", err)
		exitWithError()
	}

	// Extract the middle part of the environment name for `name`
	currentEnvironment := mainViper.GetString("environment")
	nameParts := strings.Split(currentEnvironment, "-")
	if len(nameParts) < 3 {
		pterm.Error.Println("Environment name format is invalid.")
		exitWithError()
	}
	name := nameParts[1]

	// Fetch Domain ID using the base URL and domain name
	domainID, err := fetchDomainID(baseUrl, name)
	if err != nil {
		pterm.Error.Println("Failed to fetch Domain ID:", err)
		exitWithError()
	}

	// Attempt to issue token
	accessToken, refreshToken, err := issueToken(baseUrl, userID, password, domainID)
	if err != nil {
		pterm.Error.Println("Failed to retrieve token:", err)
		exitWithError()
	}

	// Encrypt password before saving
	encryptedPassword, err := encrypt(password)
	if err != nil {
		pterm.Error.Printf("Failed to encrypt password: %v\n", err)
		exitWithError()
	}

	workspaces, err := fetchWorkspaces(baseUrl, accessToken)
	if err != nil {
		pterm.Error.Println("Failed to fetch workspaces:", err)
	}

	// Fetch Domain ID and Role Type
	domainID, roleType, err := fetchDomainIDAndRole(baseUrl, accessToken)
	if err != nil {
		pterm.Error.Println("Failed to fetch Domain ID and Role Type:", err)
		exitWithError()
	}

	// Determine scope and workspace
	scope := determineScope(roleType, len(workspaces))
	var workspaceID string
	if roleType == "DOMAIN_ADMIN" {
		workspaceID = selectScopeOrWorkspace(workspaces, roleType)
		if workspaceID == "0" {
			scope = "DOMAIN"
			workspaceID = ""
		} else {
			scope = "WORKSPACE"
		}
	} else {
		workspaceID = selectWorkspaceOnly(workspaces)
		scope = "WORKSPACE"
	}

	// Grant new token
	newAccessToken, err := grantToken(baseUrl, refreshToken, scope, domainID, workspaceID)
	if err != nil {
		pterm.Error.Println("Failed to retrieve new access token:", err)
		exitWithError()
	}

	// Save the new credentials to the configuration file
	saveCredentials(currentEnv, userID, encryptedPassword, newAccessToken)

	fmt.Println()
	pterm.Success.Println("Successfully logged in and saved token.")
}

// Prompt for user credentials if they aren't saved
func promptCredentials() (string, string) {
	userId, _ := pterm.DefaultInteractiveTextInput.Show("Enter your user ID")
	passwordInput := pterm.DefaultInteractiveTextInput.WithMask("*")
	password, _ := passwordInput.Show("Enter your password")
	return userId, password
}

// Prompt for password when token is expired
func promptPassword() string {
	passwordInput := pterm.DefaultInteractiveTextInput.WithMask("*")
	password, _ := passwordInput.Show("Enter your password")
	return password
}

// Prompt for user selection, now receiving 'users' slice as an argument
func promptUserSelection(max int, users []interface{}) int {
	if err := keyboard.Open(); err != nil {
		pterm.Error.Println("Failed to initialize keyboard:", err)
		exitWithError()
	}
	defer keyboard.Close()

	selectedIndex := 0
	currentPage := 0
	pageSize := 10
	searchMode := false
	searchTerm := ""
	filteredUsers := users

	for {
		fmt.Print("\033[H\033[2J") // Clear the screen

		// Apply search filter
		if searchTerm != "" {
			filteredUsers = filterUsers(users, searchTerm)
			if len(filteredUsers) == 0 {
				filteredUsers = users // Show all users if no search results
			}
		} else {
			filteredUsers = users
		}

		// Calculate pagination
		totalUsers := len(filteredUsers)
		totalPages := (totalUsers + pageSize - 1) / pageSize
		startIndex := currentPage * pageSize
		endIndex := startIndex + pageSize
		if endIndex > totalUsers {
			endIndex = totalUsers
		}

		// Display header with page information
		pterm.DefaultHeader.WithFullWidth().
			WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
			WithTextStyle(pterm.NewStyle(pterm.FgLightWhite)).
			Printf("Select a user account (Page %d of %d)", currentPage+1, totalPages)

		// Display existing users
		for i := startIndex; i < endIndex; i++ {
			userMap := filteredUsers[i].(map[string]interface{})
			if i == selectedIndex {
				pterm.Printf("→ %d: %s\n", i+1, userMap["userid"].(string))
			} else {
				pterm.Printf("  %d: %s\n", i+1, userMap["userid"].(string))
			}
		}

		// Display option to add new user
		if selectedIndex == totalUsers {
			pterm.Printf("→ %d: Add new user\n", totalUsers+1)
		} else {
			pterm.Printf("  %d: Add new user\n", totalUsers+1)
		}

		// Show navigation help
		pterm.DefaultBasicText.WithStyle(pterm.NewStyle(pterm.FgGray)).
			Println("\nNavigation: [h]prev-page [j]down [k]up [l]next-page [/]search [Enter]select [q]quit")

		// Show search prompt if in search mode
		if searchMode {
			fmt.Println()
			pterm.Info.Printf("Search (ESC to cancel, Enter to confirm): %s", searchTerm)
		}

		// Get keyboard input
		char, key, err := keyboard.GetKey()
		if err != nil {
			pterm.Error.Println("Error reading keyboard input:", err)
			exitWithError()
		}

		// Handle search mode input
		if searchMode {
			switch key {
			case keyboard.KeyEsc:
				searchMode = false
				searchTerm = ""
				filteredUsers = users // Return to full user list when search term is cleared
			case keyboard.KeyBackspace, keyboard.KeyBackspace2:
				if len(searchTerm) > 0 {
					searchTerm = searchTerm[:len(searchTerm)-1]
				}
			case keyboard.KeyEnter:
				searchMode = false
			default:
				if char != 0 {
					searchTerm += string(char)
				}
			}
			currentPage = 0
			selectedIndex = 0
			continue
		}

		// Handle normal mode input
		switch key {
		case keyboard.KeyEnter:
			if selectedIndex <= len(filteredUsers) {
				// If "Add new user" is selected
				if selectedIndex == len(filteredUsers) {
					return len(users) + 1
				}
				// Find the original index of the selected user
				selectedUserMap := filteredUsers[selectedIndex].(map[string]interface{})
				selectedUserID := selectedUserMap["userid"].(string)

				for i, user := range users {
					userMap := user.(map[string]interface{})
					if userMap["userid"].(string) == selectedUserID {
						return i + 1
					}
				}
			}
		}

		switch char {
		case 'j': // Down
			if selectedIndex < min(endIndex, totalUsers) {
				selectedIndex++
			}
		case 'k': // Up
			if selectedIndex > startIndex {
				selectedIndex--
			}
		case 'l': // Next page
			if currentPage < totalPages-1 {
				currentPage++
				selectedIndex = currentPage * pageSize
			}
		case 'h': // Previous page
			if currentPage > 0 {
				currentPage--
				selectedIndex = currentPage * pageSize
			}
		case '/': // Enter search mode
			searchMode = true
			searchTerm = ""
			selectedIndex = 0
		case 'q', 'Q':
			fmt.Println()
			pterm.Error.Println("User selection cancelled.")
			os.Exit(1)
		}
	}
}

// filterUsers filters the users list based on the search term
func filterUsers(users []interface{}, searchTerm string) []interface{} {
	var filtered []interface{}
	searchTerm = strings.ToLower(searchTerm)

	for _, user := range users {
		userMap := user.(map[string]interface{})
		userid := strings.ToLower(userMap["userid"].(string))
		if strings.Contains(userid, searchTerm) {
			filtered = append(filtered, user)
		}
	}
	return filtered
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func getEncryptionKey() ([]byte, error) {
	key, err := keyring.Get(keyringService, keyringUser)
	if err == keyring.ErrNotFound {
		newKey := make([]byte, 32)
		if _, err := rand.Read(newKey); err != nil {
			return nil, fmt.Errorf("failed to generate new key: %v", err)
		}

		encodedKey := base64.StdEncoding.EncodeToString(newKey)
		if err := keyring.Set(keyringService, keyringUser, encodedKey); err != nil {
			return nil, fmt.Errorf("failed to store key in keychain: %v", err)
		}

		return newKey, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to access keychain: %v", err)
	}

	return base64.StdEncoding.DecodeString(key)
}

func encrypt(text string) (string, error) {
	key, err := getEncryptionKey()
	if err != nil {
		return "", fmt.Errorf("failed to get encryption key: %v", err)
	}

	plaintext := []byte(text)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	ciphertext := make([]byte, aes.BlockSize+len(plaintext))
	iv := ciphertext[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}

	stream := cipher.NewCFBEncrypter(block, iv)
	stream.XORKeyStream(ciphertext[aes.BlockSize:], plaintext)

	return base64.URLEncoding.EncodeToString(ciphertext), nil
}

func decrypt(cryptoText string) (string, error) {
	key, err := getEncryptionKey()
	if err != nil {
		return "", fmt.Errorf("failed to get encryption key: %v", err)
	}

	ciphertext, err := base64.URLEncoding.DecodeString(cryptoText)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	if len(ciphertext) < aes.BlockSize {
		return "", errors.New("ciphertext too short")
	}

	iv := ciphertext[:aes.BlockSize]
	ciphertext = ciphertext[aes.BlockSize:]

	stream := cipher.NewCFBDecrypter(block, iv)
	stream.XORKeyStream(ciphertext, ciphertext)

	return string(ciphertext), nil
}

// Define a struct for user credentials
type UserCredentials struct {
	UserID   string `yaml:"userid"`
	Password string `yaml:"password"`
	Token    string `yaml:"token"`
}

// saveCredentials saves the user's credentials to the configuration
func saveCredentials(currentEnv, userID, password, token string) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		pterm.Error.Printf("Failed to get user home directory: %v\n", err)
		return
	}

	cacheDir := filepath.Join(homeDir, ".cfctl", "cache")
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		pterm.Error.Printf("Failed to create cache directory: %v\n", err)
		return
	}

	cacheConfigPath := filepath.Join(cacheDir, "config.yaml")
	cacheViper := viper.New()
	cacheViper.SetConfigFile(cacheConfigPath)

	if err := cacheViper.ReadInConfig(); err != nil && !os.IsNotExist(err) {
		pterm.Error.Printf("Failed to read cache config: %v\n", err)
		return
	}

	envPath := fmt.Sprintf("environments.%s", currentEnv)
	envSettings := cacheViper.GetStringMap(envPath)
	if envSettings == nil {
		envSettings = make(map[string]interface{})
	}

	var users []UserCredentials
	if existingUsers, ok := envSettings["users"]; ok {
		if userList, ok := existingUsers.([]interface{}); ok {
			for _, u := range userList {
				if userMap, ok := u.(map[string]interface{}); ok {
					user := UserCredentials{
						UserID:   userMap["userid"].(string),
						Password: userMap["password"].(string),
						Token:    userMap["token"].(string),
					}
					users = append(users, user)
				}
			}
		}
	}

	// Update existing user or add new user
	userExists := false
	for i, user := range users {
		if user.UserID == userID {
			users[i].Password = password
			users[i].Token = token
			userExists = true
			break
		}
	}

	if !userExists {
		newUser := UserCredentials{
			UserID:   userID,
			Password: password,
			Token:    token,
		}
		users = append(users, newUser)
	}

	envSettings["users"] = users
	cacheViper.Set(envPath, envSettings)

	if err := cacheViper.WriteConfig(); err != nil {
		pterm.Error.Printf("Failed to save user credentials: %v\n", err)
	}
}

func verifyAppToken(token string) (map[string]interface{}, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		pterm.Error.Println("Invalid token format")
		return nil, false
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		pterm.Error.Println("Failed to decode token payload:", err)
		return nil, false
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		pterm.Error.Println("Failed to parse token payload:", err)
		return nil, false
	}

	exp, ok := claims["exp"].(float64)
	if !ok {
		pterm.Error.Println("Expiration time not found in token")
		return nil, false
	}

	if time.Now().After(time.Unix(int64(exp), 0)) {
		pterm.DefaultBox.WithTitle("Expired App Token").
			WithTitleTopCenter().
			WithRightPadding(4).
			WithLeftPadding(4).
			WithBoxStyle(pterm.NewStyle(pterm.FgRed)).
			Println("Your App token has expired.\nPlease generate a new App and update your config file.")
		return nil, false
	}

	role, ok := claims["rol"].(string)
	if !ok {
		pterm.Error.Println("Role not found in token")
		return nil, false
	}

	if role != "DOMAIN_ADMIN" && role != "WORKSPACE_OWNER" {
		pterm.DefaultBox.WithTitle("Invalid App Token").
			WithTitleTopCenter().
			WithRightPadding(4).
			WithLeftPadding(4).
			WithBoxStyle(pterm.NewStyle(pterm.FgRed)).
			Println("App token must have either DOMAIN_ADMIN or WORKSPACE_OWNER role.\nPlease generate a new App with appropriate permissions and update your config file.")
		return nil, false
	}

	return claims, true
}

// Load environment-specific configuration based on the selected environment
func loadEnvironmentConfig() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		pterm.Error.Println("Failed to get user home directory:", err)
		exitWithError()
	}

	mainConfigPath := filepath.Join(homeDir, ".cfctl", "config.yaml")
	cacheConfigPath := filepath.Join(homeDir, ".cfctl", "cache", "config.yaml")

	viper.SetConfigFile(mainConfigPath)
	if err := viper.ReadInConfig(); err != nil {
		pterm.Error.Println("Failed to read config.yaml:", err)
		exitWithError()
	}

	currentEnvironment := viper.GetString("environment")
	if currentEnvironment == "" {
		pterm.Error.Println("No environment specified in config.yaml")
		exitWithError()
	}

	configFound := false
	for _, configPath := range []string{mainConfigPath, cacheConfigPath} {
		v := viper.New()
		v.SetConfigFile(configPath)
		if err := v.ReadInConfig(); err == nil {
			endpointKey := fmt.Sprintf("environments.%s.endpoint", currentEnvironment)
			tokenKey := fmt.Sprintf("environments.%s.token", currentEnvironment)

			if providedUrl == "" {
				providedUrl = v.GetString(endpointKey)
			}

			if token := v.GetString(tokenKey); token != "" {
				viper.Set("token", token)
			}

			if providedUrl != "" {
				configFound = true
			}
		}
	}

	if !configFound {
		pterm.Error.Printf("No endpoint found for the current environment '%s'\n", currentEnvironment)
		exitWithError()
	}

	isProxyEnabled := viper.GetBool(fmt.Sprintf("environments.%s.proxy", currentEnvironment))
	containsIdentity := strings.Contains(strings.ToLower(providedUrl), "identity")

	if !isProxyEnabled && !containsIdentity {
		pterm.DefaultBox.WithTitle("Proxy Mode Required").
			WithTitleTopCenter().
			WithBoxStyle(pterm.NewStyle(pterm.FgYellow)).
			Println("Current endpoint is not configured for identity service.\n" +
				"Please enable proxy mode and set identity endpoint first.")

		pterm.DefaultBox.WithBoxStyle(pterm.NewStyle(pterm.FgCyan)).
			Println("$ cfctl config endpoint -s identity\n" +
				"$ cfctl login")

		exitWithError()
	}
}

func determineScope(roleType string, workspaceCount int) string {
	switch roleType {
	case "DOMAIN_ADMIN":
		return "DOMAIN"
	case "WORKSPACE_OWNER", "WORKSPACE_MEMBER", "USER":
		return "WORKSPACE"
	default:
		pterm.Error.Println("Unknown role_type:", roleType)
		exitWithError()
		return "" // Unreachable
	}
}

func isTokenExpired(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		pterm.Error.Println("Invalid token format.")
		return true
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		pterm.Error.Println("Failed to decode token payload:", err)
		return true
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		pterm.Error.Println("Failed to unmarshal token payload:", err)
		return true
	}

	exp, ok := claims["exp"].(float64)
	if !ok {
		pterm.Error.Println("Expiration time (exp) not found in token.")
		return true
	}

	expirationTime := time.Unix(int64(exp), 0)
	return time.Now().After(expirationTime)
}

func verifyToken(token string) bool {
	// This function should implement token verification logic, for example by making a request to an endpoint that requires authentication
	// Returning true for simplicity in this example
	return true
}

func exitWithError() {
	os.Exit(1)
}

func fetchDomainID(baseUrl string, name string) (string, error) {
	// Parse the endpoint
	parts := strings.Split(baseUrl, "://")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid endpoint format: %s", baseUrl)
	}

	hostPort := parts[1]

	// Configure gRPC connection
	var opts []grpc.DialOption
	if strings.HasPrefix(baseUrl, "grpc+ssl://") {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Establish connection
	conn, err := grpc.Dial(hostPort, opts...)
	if err != nil {
		return "", fmt.Errorf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Create reflection client
	refClient := grpcreflect.NewClient(context.Background(), grpc_reflection_v1alpha.NewServerReflectionClient(conn))
	defer refClient.Reset()

	// Resolve the service
	serviceName := "spaceone.api.identity.v2.Domain"
	serviceDesc, err := refClient.ResolveService(serviceName)
	if err != nil {
		return "", fmt.Errorf("failed to resolve service %s: %v", serviceName, err)
	}

	// Find the method descriptor
	methodDesc := serviceDesc.FindMethodByName("get_auth_info")
	if methodDesc == nil {
		return "", fmt.Errorf("method get_auth_info not found")
	}

	// Create request message
	reqMsg := dynamic.NewMessage(methodDesc.GetInputType())
	reqMsg.SetFieldByName("name", name)

	// Make the gRPC call
	fullMethod := fmt.Sprintf("/%s/%s", serviceName, "get_auth_info")
	respMsg := dynamic.NewMessage(methodDesc.GetOutputType())

	err = conn.Invoke(context.Background(), fullMethod, reqMsg, respMsg)
	if err != nil {
		return "", fmt.Errorf("RPC failed: %v", err)
	}

	// Extract domain_id from response
	domainID, err := respMsg.TryGetFieldByName("domain_id")
	if err != nil {
		return "", fmt.Errorf("failed to get domain_id from response: %v", err)
	}

	return domainID.(string), nil
}

func issueToken(baseUrl, userID, password, domainID string) (string, string, error) {
	// Parse the endpoint
	parts := strings.Split(baseUrl, "://")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid endpoint format: %s", baseUrl)
	}

	hostPort := parts[1]

	// Configure gRPC connection
	var opts []grpc.DialOption
	if strings.HasPrefix(baseUrl, "grpc+ssl://") {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Establish connection
	conn, err := grpc.Dial(hostPort, opts...)
	if err != nil {
		return "", "", fmt.Errorf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Create reflection client
	refClient := grpcreflect.NewClient(context.Background(), grpc_reflection_v1alpha.NewServerReflectionClient(conn))
	defer refClient.Reset()

	// Resolve the service
	serviceName := "spaceone.api.identity.v2.Token"
	serviceDesc, err := refClient.ResolveService(serviceName)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve service %s: %v", serviceName, err)
	}

	// Find the method descriptor
	methodDesc := serviceDesc.FindMethodByName("issue")
	if methodDesc == nil {
		return "", "", fmt.Errorf("method issue not found")
	}

	// Create request message
	reqMsg := dynamic.NewMessage(methodDesc.GetInputType())

	// Create credentials struct using protobuf types
	structpb := &structpb.Struct{
		Fields: map[string]*structpb.Value{
			"user_id": {
				Kind: &structpb.Value_StringValue{
					StringValue: userID,
				},
			},
			"password": {
				Kind: &structpb.Value_StringValue{
					StringValue: password,
				},
			},
		},
	}

	// Set all fields in the request message
	reqMsg.SetFieldByName("credentials", structpb)
	reqMsg.SetFieldByName("auth_type", int32(1)) // LOCAL = 1
	reqMsg.SetFieldByName("timeout", int32(0))
	reqMsg.SetFieldByName("verify_code", "")
	reqMsg.SetFieldByName("domain_id", domainID)

	// Make the gRPC call
	fullMethod := fmt.Sprintf("/%s/%s", serviceName, "issue")
	respMsg := dynamic.NewMessage(methodDesc.GetOutputType())

	err = conn.Invoke(context.Background(), fullMethod, reqMsg, respMsg)
	if err != nil {
		return "", "", fmt.Errorf("RPC failed: %v", err)
	}

	// Extract tokens from response
	accessToken, err := respMsg.TryGetFieldByName("access_token")
	if err != nil {
		return "", "", fmt.Errorf("failed to get access_token from response: %v", err)
	}

	refreshToken, err := respMsg.TryGetFieldByName("refresh_token")
	if err != nil {
		return "", "", fmt.Errorf("failed to get refresh_token from response: %v", err)
	}

	return accessToken.(string), refreshToken.(string), nil
}

func fetchWorkspaces(baseUrl string, accessToken string) ([]map[string]interface{}, error) {
	// Parse the endpoint
	parts := strings.Split(baseUrl, "://")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid endpoint format: %s", baseUrl)
	}

	hostPort := parts[1]

	// Configure gRPC connection
	var opts []grpc.DialOption
	if strings.HasPrefix(baseUrl, "grpc+ssl://") {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Add token credentials
	creds := &tokenAuth{
		token: accessToken,
	}
	opts = append(opts, grpc.WithPerRPCCredentials(creds))

	// Establish connection
	conn, err := grpc.Dial(hostPort, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Create reflection client
	refClient := grpcreflect.NewClient(context.Background(), grpc_reflection_v1alpha.NewServerReflectionClient(conn))
	defer refClient.Reset()

	// Resolve the service
	serviceName := "spaceone.api.identity.v2.UserProfile"
	serviceDesc, err := refClient.ResolveService(serviceName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve service %s: %v", serviceName, err)
	}

	// Find the method descriptor
	methodDesc := serviceDesc.FindMethodByName("get_workspaces")
	if methodDesc == nil {
		return nil, fmt.Errorf("method get_workspaces not found")
	}

	// Create request message
	reqMsg := dynamic.NewMessage(methodDesc.GetInputType())

	// Create metadata with token
	md := metadata.New(map[string]string{
		"token": accessToken,
	})
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	// Make the gRPC call
	fullMethod := "/spaceone.api.identity.v2.UserProfile/get_workspaces"
	respMsg := dynamic.NewMessage(methodDesc.GetOutputType())

	err = conn.Invoke(ctx, fullMethod, reqMsg, respMsg)
	if err != nil {
		return nil, fmt.Errorf("RPC failed: %v", err)
	}

	// Extract results from response
	results, err := respMsg.TryGetFieldByName("results")
	if err != nil {
		return nil, fmt.Errorf("failed to get results from response: %v", err)
	}

	workspaces, ok := results.([]interface{})
	if !ok || len(workspaces) == 0 {
		pterm.Warning.Println("There are no accessible workspaces. Ask your administrators or workspace owners for access.")
		exitWithError()
	}

	var workspaceList []map[string]interface{}
	for _, workspace := range workspaces {
		workspaceMsg, ok := workspace.(*dynamic.Message)
		if !ok {
			return nil, fmt.Errorf("failed to parse workspace message")
		}

		workspaceMap := make(map[string]interface{})
		fields := workspaceMsg.GetKnownFields()

		for _, field := range fields {
			if value, err := workspaceMsg.TryGetFieldByName(field.GetName()); err == nil {
				workspaceMap[field.GetName()] = value
			}
		}

		workspaceList = append(workspaceList, workspaceMap)
	}

	return workspaceList, nil
}

func fetchDomainIDAndRole(baseUrl string, accessToken string) (string, string, error) {
	// Parse the endpoint
	parts := strings.Split(baseUrl, "://")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid endpoint format: %s", baseUrl)
	}

	hostPort := parts[1]

	// Configure gRPC connection
	var opts []grpc.DialOption
	if strings.HasPrefix(baseUrl, "grpc+ssl://") {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Add token to metadata
	opts = append(opts, grpc.WithPerRPCCredentials(&tokenAuth{token: accessToken}))

	// Establish connection
	conn, err := grpc.Dial(hostPort, opts...)
	if err != nil {
		return "", "", fmt.Errorf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Create reflection client
	refClient := grpcreflect.NewClient(context.Background(), grpc_reflection_v1alpha.NewServerReflectionClient(conn))
	defer refClient.Reset()

	// Resolve the service
	serviceName := "spaceone.api.identity.v2.UserProfile"
	serviceDesc, err := refClient.ResolveService(serviceName)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve service %s: %v", serviceName, err)
	}

	// Find the method descriptor
	methodDesc := serviceDesc.FindMethodByName("get")
	if methodDesc == nil {
		return "", "", fmt.Errorf("method get not found")
	}

	// Create request message
	reqMsg := dynamic.NewMessage(methodDesc.GetInputType())

	// Make the gRPC call
	fullMethod := fmt.Sprintf("/%s/%s", serviceName, "get")
	respMsg := dynamic.NewMessage(methodDesc.GetOutputType())

	err = conn.Invoke(context.Background(), fullMethod, reqMsg, respMsg)
	if err != nil {
		return "", "", fmt.Errorf("RPC failed: %v", err)
	}

	// Extract domain_id and role_type from response
	domainID, err := respMsg.TryGetFieldByName("domain_id")
	if err != nil {
		return "", "", fmt.Errorf("failed to get domain_id from response: %v", err)
	}

	roleType, err := respMsg.TryGetFieldByName("role_type")
	if err != nil {
		return "", "", fmt.Errorf("failed to get role_type from response: %v", err)
	}

	// Convert roleType to string based on enum value
	var roleTypeStr string
	switch v := roleType.(type) {
	case int32:
		switch v {
		case 1:
			roleTypeStr = "DOMAIN_ADMIN"
		case 2:
			roleTypeStr = "WORKSPACE_OWNER"
		case 3:
			roleTypeStr = "WORKSPACE_MEMBER"
		default:
			return "", "", fmt.Errorf("unknown role_type: %d", v)
		}
	case string:
		roleTypeStr = v
	default:
		return "", "", fmt.Errorf("unexpected role_type type: %T", roleType)
	}

	return domainID.(string), roleTypeStr, nil
}

func grantToken(baseUrl, refreshToken, scope, domainID, workspaceID string) (string, error) {
	// Parse the endpoint
	parts := strings.Split(baseUrl, "://")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid endpoint format: %s", baseUrl)
	}

	hostPort := parts[1]

	// Configure gRPC connection
	var opts []grpc.DialOption
	if strings.HasPrefix(baseUrl, "grpc+ssl://") {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Establish connection
	conn, err := grpc.Dial(hostPort, opts...)
	if err != nil {
		return "", fmt.Errorf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Create reflection client
	refClient := grpcreflect.NewClient(context.Background(), grpc_reflection_v1alpha.NewServerReflectionClient(conn))
	defer refClient.Reset()

	// Resolve the service
	serviceName := "spaceone.api.identity.v2.Token"
	serviceDesc, err := refClient.ResolveService(serviceName)
	if err != nil {
		return "", fmt.Errorf("failed to resolve service %s: %v", serviceName, err)
	}

	// Find the method descriptor
	methodDesc := serviceDesc.FindMethodByName("grant")
	if methodDesc == nil {
		return "", fmt.Errorf("method grant not found")
	}

	// Create request message
	reqMsg := dynamic.NewMessage(methodDesc.GetInputType())

	reqMsg.SetFieldByName("grant_type", int32(1))

	var scopeEnum int32
	switch scope {
	case "DOMAIN":
		scopeEnum = 2
	case "WORKSPACE":
		scopeEnum = 3
	case "USER":
		scopeEnum = 5
	default:
		return "", fmt.Errorf("unknown scope: %s", scope)
	}

	reqMsg.SetFieldByName("scope", scopeEnum)
	reqMsg.SetFieldByName("token", refreshToken)
	reqMsg.SetFieldByName("timeout", int32(86400))
	reqMsg.SetFieldByName("domain_id", domainID)
	if workspaceID != "" {
		reqMsg.SetFieldByName("workspace_id", workspaceID)
	}

	// Make the gRPC call
	fullMethod := "/spaceone.api.identity.v2.Token/grant"
	respMsg := dynamic.NewMessage(methodDesc.GetOutputType())

	err = conn.Invoke(context.Background(), fullMethod, reqMsg, respMsg)
	if err != nil {
		return "", fmt.Errorf("RPC failed: %v", err)
	}

	// Extract access_token from response
	accessToken, err := respMsg.TryGetFieldByName("access_token")
	if err != nil {
		return "", fmt.Errorf("failed to get access_token from response: %v", err)
	}

	return accessToken.(string), nil
}

// saveSelectedToken saves the selected token as the current token for the environment
func saveSelectedToken(currentEnv, selectedToken string) error {
	homeDir, _ := os.UserHomeDir()
	configPath := filepath.Join(homeDir, ".cfctl", "config.yaml")

	viper.SetConfigFile(configPath)
	if err := viper.ReadInConfig(); err != nil && !os.IsNotExist(err) {
		return err
	}

	envPath := fmt.Sprintf("environments.%s", currentEnv)
	envSettings := viper.GetStringMap(envPath)
	if envSettings == nil {
		envSettings = make(map[string]interface{})
	}

	// Keep only endpoint and proxy settings
	newEnvSettings := make(map[string]interface{})
	if endpoint, ok := envSettings["endpoint"]; ok {
		newEnvSettings["endpoint"] = endpoint
	}
	if proxy, ok := envSettings["proxy"]; ok {
		newEnvSettings["proxy"] = proxy
	}

	// Keep the tokens array
	if tokens, ok := envSettings["tokens"]; ok {
		newEnvSettings["tokens"] = tokens
	}

	viper.Set(envPath, newEnvSettings)
	return viper.WriteConfig()
}

func selectScopeOrWorkspace(workspaces []map[string]interface{}, roleType string) string {
	if err := keyboard.Open(); err != nil {
		pterm.Error.Println("Failed to initialize keyboard:", err)
		exitWithError()
	}
	defer keyboard.Close()

	if roleType != "DOMAIN_ADMIN" {
		return selectWorkspaceOnly(workspaces)
	}

	options := []string{"DOMAIN", "WORKSPACES"}
	selectedIndex := 0

	for {
		fmt.Print("\033[H\033[2J")

		// Display scope selection
		pterm.DefaultHeader.WithFullWidth().
			WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
			WithTextStyle(pterm.NewStyle(pterm.FgLightWhite)).
			Println("Select Scope")

		for i, option := range options {
			if i == selectedIndex {
				pterm.Printf("→ %d: %s\n", i, option)
			} else {
				pterm.Printf("  %d: %s\n", i, option)
			}
		}

		// Show navigation help
		pterm.DefaultBasicText.WithStyle(pterm.NewStyle(pterm.FgGray)).
			Println("\nNavigation: [j]down [k]up, [Enter]select, [q]uit")

		// Get keyboard input
		char, key, err := keyboard.GetKey()
		if err != nil {
			pterm.Error.Println("Error reading keyboard input:", err)
			exitWithError()
		}

		// Handle navigation and other commands
		switch key {
		case keyboard.KeyEnter:
			if selectedIndex == 0 {
				return "0"
			} else {
				return selectWorkspaceOnly(workspaces)
			}
		}

		switch char {
		case 'j': // Down
			if selectedIndex < len(options)-1 {
				selectedIndex++
			}
		case 'k': // Up
			if selectedIndex > 0 {
				selectedIndex--
			}
		case 'q', 'Q':
			pterm.Error.Println("Selection cancelled.")
			os.Exit(1)
		}
	}
}

// selectWorkspaceOnly handles workspace selection
func selectWorkspaceOnly(workspaces []map[string]interface{}) string {
	const pageSize = 15
	currentPage := 0
	searchMode := false
	searchTerm := ""
	selectedIndex := 0
	inputBuffer := ""
	filteredWorkspaces := workspaces

	if err := keyboard.Open(); err != nil {
		pterm.Error.Println("Failed to initialize keyboard:", err)
		exitWithError()
	}
	defer keyboard.Close()

	for {
		// Clear screen
		fmt.Print("\033[H\033[2J")

		// Apply search filter
		if searchTerm != "" {
			filteredWorkspaces = filterWorkspaces(workspaces, searchTerm)
			if len(filteredWorkspaces) == 0 {
				filteredWorkspaces = workspaces
			}
		} else {
			filteredWorkspaces = workspaces
		}

		// Calculate pagination
		totalWorkspaces := len(filteredWorkspaces)
		totalPages := (totalWorkspaces + pageSize - 1) / pageSize
		startIndex := (currentPage % totalPages) * pageSize
		endIndex := startIndex + pageSize
		if endIndex > totalWorkspaces {
			endIndex = totalWorkspaces
		}

		// Display header with page information
		pterm.DefaultHeader.WithFullWidth().
			WithBackgroundStyle(pterm.NewStyle(pterm.BgDarkGray)).
			WithTextStyle(pterm.NewStyle(pterm.FgLightWhite)).
			Printf("Accessible Workspaces (Page %d of %d)", currentPage+1, totalPages)

		// Show workspace list
		for i := startIndex; i < endIndex; i++ {
			name := filteredWorkspaces[i]["name"].(string)
			if i-startIndex == selectedIndex {
				pterm.Printf("→ %d: %s\n", i+1, name)
			} else {
				pterm.Printf("  %d: %s\n", i+1, name)
			}
		}

		// Show navigation help and search prompt
		pterm.DefaultBasicText.WithStyle(pterm.NewStyle(pterm.FgGray)).
			Println("\nNavigation: [h]prev-page [j]down [k]up  [l]next-page [/]search [q]uit")

		// Show search or input prompt at the bottom
		if searchMode {
			fmt.Println()
			pterm.Info.Printf("Search (ESC to cancel, Enter to confirm): %s", searchTerm)
		} else {
			fmt.Print("\nSelect a workspace above or input a number: ")
			if inputBuffer != "" {
				fmt.Print(inputBuffer)
			}
		}

		// Get keyboard input
		char, key, err := keyboard.GetKey()
		if err != nil {
			pterm.Error.Println("Error reading keyboard input:", err)
			exitWithError()
		}

		// Handle search mode input
		if searchMode {
			switch key {
			case keyboard.KeyEsc:
				searchMode = false
				searchTerm = ""
			case keyboard.KeyBackspace, keyboard.KeyBackspace2:
				if len(searchTerm) > 0 {
					searchTerm = searchTerm[:len(searchTerm)-1]
				}
			case keyboard.KeyEnter:
				searchMode = false
			default:
				if char != 0 {
					searchTerm += string(char)
				}
			}
			currentPage = 0
			selectedIndex = 0
			continue
		}

		// Handle normal mode input
		switch key {
		case keyboard.KeyEnter:
			if inputBuffer != "" {
				index, err := strconv.Atoi(inputBuffer)
				if err == nil && index >= 1 && index <= len(filteredWorkspaces) {
					return filteredWorkspaces[index-1]["workspace_id"].(string)
				}
				inputBuffer = ""
			} else {
				adjustedIndex := startIndex + selectedIndex
				if adjustedIndex >= 0 && adjustedIndex < len(filteredWorkspaces) {
					return filteredWorkspaces[adjustedIndex]["workspace_id"].(string)
				}
			}
		case keyboard.KeyBackspace, keyboard.KeyBackspace2:
			if len(inputBuffer) > 0 {
				inputBuffer = inputBuffer[:len(inputBuffer)-1]
			}
		}

		// Handle navigation and other commands
		switch char {
		case 'j': // Down
			if selectedIndex < min(pageSize-1, endIndex-startIndex-1) {
				selectedIndex++
			}
		case 'k': // Up
			if selectedIndex > 0 {
				selectedIndex--
			}
		case 'l': // Next page
			currentPage = (currentPage + 1) % totalPages
			selectedIndex = 0
		case 'h': // Previous page
			currentPage = (currentPage - 1 + totalPages) % totalPages
			selectedIndex = 0
		case 'q', 'Q':
			fmt.Println()
			pterm.Error.Println("Workspace selection cancelled.")
			os.Exit(1)
		case '/':
			searchMode = true
			searchTerm = ""
			selectedIndex = 0
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			if !searchMode {
				inputBuffer += string(char)
			}
		}
	}
}

func filterWorkspaces(workspaces []map[string]interface{}, searchTerm string) []map[string]interface{} {
	var filtered []map[string]interface{}
	searchTerm = strings.ToLower(searchTerm)

	for _, workspace := range workspaces {
		name := strings.ToLower(workspace["name"].(string))
		if strings.Contains(name, searchTerm) {
			filtered = append(filtered, workspace)
		}
	}
	return filtered
}

func init() {
	LoginCmd.Flags().StringVarP(&providedUrl, "url", "u", "", "The URL to use for login (e.g. cfctl login -u https://example.com)")
}
