package other

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jhump/protoreflect/dynamic"

	"github.com/jhump/protoreflect/grpcreflect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection/grpc_reflection_v1alpha"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// SettingCmd represents the setting command
var SettingCmd = &cobra.Command{
	Use:   "setting",
	Short: "Manage cfctl setting file",
	Long: `Manage setting file for cfctl. 
You can initialize, switch environments, and display the current configuration.`,
}

// settingInitCmd initializes a new environment configuration
var settingInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new environment setting",
	Long:  `Initialize a new environment setting for cfctl by specifying an endpoint`,
	Example: `  cfctl setting init endpoint https://example.com --app
  cfctl setting init endpoint https://example.com --user
  cfctl setting init endpoint http://localhost:8080 --app
  cfctl setting init endpoint http://localhost:8080 --user
	                         or 
  cfctl setting init local`,
}

// settingInitLocalCmd represents the setting init local command
var settingInitLocalCmd = &cobra.Command{
	Use:   "local",
	Short: "Initialize local environment setting",
	Long:  `Initialize a local environment setting with default configuration.`,
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		settingDir := GetSettingDir()
		if err := os.MkdirAll(settingDir, 0755); err != nil {
			pterm.Error.Printf("Failed to create setting directory: %v\n", err)
			return
		}

		mainSettingPath := filepath.Join(settingDir, "setting.yaml")
		v := viper.New()
		v.SetConfigFile(mainSettingPath)
		v.SetConfigType("yaml")

		// Check if local environment already exists
		if err := v.ReadInConfig(); err == nil {
			environments := v.GetStringMap("environments")
			if existingEnv, exists := environments["local"]; exists {
				currentConfig, _ := yaml.Marshal(map[string]interface{}{
					"environment": "local",
					"environments": map[string]interface{}{
						"local": existingEnv,
					},
				})

				confirmBox := pterm.DefaultBox.WithTitle("Environment Already Exists").
					WithTitleTopCenter().
					WithRightPadding(4).
					WithLeftPadding(4).
					WithBoxStyle(pterm.NewStyle(pterm.FgYellow))

				confirmBox.Println("Environment 'local' already exists.\nDo you want to overwrite it?")

				pterm.Info.Println("Current configuration:")
				fmt.Println(string(currentConfig))

				fmt.Print("\nEnter (y/n): ")
				var response string
				fmt.Scanln(&response)
				response = strings.ToLower(strings.TrimSpace(response))

				if response != "y" {
					pterm.Info.Println("Operation cancelled. Environment 'local' remains unchanged.")
					return
				}
			}
		}

		updateSetting("local", "grpc://localhost:50051", "")
	},
}

// settingInitEndpointCmd represents the setting init endpoint command
var settingInitEndpointCmd = &cobra.Command{
	Use:   "endpoint [URL]",
	Short: "Initialize configuration with an endpoint",
	Long:  `Specify an endpoint to initialize the environment configuration.`,
	Args:  cobra.ExactArgs(1),
	Example: `  cfctl setting init endpoint https://example.com --app
  cfctl setting init endpoint https://example.com --user
  cfctl setting init endpoint http://localhost:8080 --app
  cfctl setting init endpoint http://localhost:8080 --user`,
	Run: func(cmd *cobra.Command, args []string) {
		endpointStr := args[0]
		appFlag, _ := cmd.Flags().GetBool("app")
		userFlag, _ := cmd.Flags().GetBool("user")

		if !appFlag && !userFlag {
			pterm.Error.Println("You must specify either --app or --user flag.")
			cmd.Help()
			return
		}

		envName, err := parseEnvNameFromURL(endpointStr)
		if err != nil {
			pterm.Error.Printf("Failed to parse environment name from endpoint: %v\n", err)
			return
		}

		settingDir := GetSettingDir()
		if err := os.MkdirAll(settingDir, 0755); err != nil {
			pterm.Error.Printf("Failed to create setting directory: %v\n", err)
			return
		}

		mainSettingPath := filepath.Join(settingDir, "setting.yaml")
		v := viper.New()
		v.SetConfigFile(mainSettingPath)
		v.SetConfigType("yaml")

		envSuffix := map[bool]string{true: "app", false: "user"}[appFlag]
		fullEnvName := fmt.Sprintf("%s-%s", envName, envSuffix)

		if err := v.ReadInConfig(); err == nil {
			environments := v.GetStringMap("environments")
			if existingEnv, exists := environments[fullEnvName]; exists {
				currentConfig, _ := yaml.Marshal(map[string]interface{}{
					"environment": fullEnvName,
					"environments": map[string]interface{}{
						fullEnvName: existingEnv,
					},
				})

				confirmBox := pterm.DefaultBox.WithTitle("Environment Already Exists").
					WithTitleTopCenter().
					WithRightPadding(4).
					WithLeftPadding(4).
					WithBoxStyle(pterm.NewStyle(pterm.FgYellow))

				confirmBox.Println(fmt.Sprintf("Environment '%s' already exists.\nDo you want to overwrite it?", fullEnvName))

				pterm.Info.Println("Current configuration:")
				fmt.Println(string(currentConfig))

				fmt.Print("\nEnter (y/n): ")
				var response string
				fmt.Scanln(&response)
				response = strings.ToLower(strings.TrimSpace(response))

				if response != "y" {
					pterm.Info.Printf("Operation cancelled. Environment '%s' remains unchanged.\n", fullEnvName)
					return
				}
			}
		}

		// Update configuration
		updateSetting(envName, endpointStr, envSuffix)
	},
}

// envCmd manages environment switching and listing
var envCmd = &cobra.Command{
	Use:   "environment",
	Short: "List and manage environments",
	Long:  "List and manage environments",
	Run: func(cmd *cobra.Command, args []string) {
		// Set paths for app and user configurations
		settingDir := GetSettingDir()
		appSettingPath := filepath.Join(settingDir, "setting.yaml")

		// Create separate Viper instances
		appV := viper.New()

		// Load app configuration
		if err := loadSetting(appV, appSettingPath); err != nil {
			pterm.Error.Println(err)
			return
		}

		// Get current environment (from app setting only)
		currentEnv := getCurrentEnvironment(appV)

		// Check if -s or -r flag is provided
		switchEnv, _ := cmd.Flags().GetString("switch")
		removeEnv, _ := cmd.Flags().GetString("remove")

		// Handle environment switching (app setting only)
		if switchEnv != "" {
			// Check environment in both app and user settings
			appEnvMap := appV.GetStringMap("environments")

			if currentEnv == switchEnv {
				pterm.Info.Printf("Already in '%s' environment.\n", currentEnv)
				return
			}

			if _, existsApp := appEnvMap[switchEnv]; !existsApp {
				home, _ := os.UserHomeDir()
				pterm.Error.Printf("Environment '%s' not found in %s/.cfctl/setting.yaml",
					switchEnv, home)
				return
			}

			// Update only the environment field in app setting
			appV.Set("environment", switchEnv)

			if err := appV.WriteConfig(); err != nil {
				pterm.Error.Printf("Failed to update environment in setting.yaml: %v", err)
				return
			}

			pterm.Success.Printf("Switched to '%s' environment.\n", switchEnv)
			updateGlobalSetting()
			return
		}

		// Handle environment removal with confirmation
		if removeEnv != "" {
			// Determine which Viper instance contains the environment
			var targetViper *viper.Viper
			var targetSettingPath string
			envMapApp := appV.GetStringMap("environments")

			if _, exists := envMapApp[removeEnv]; exists {
				targetViper = appV
				targetSettingPath = appSettingPath
			} else {
				home, _ := os.UserHomeDir()
				pterm.Error.Printf("Environment '%s' not found in %s/.cfctl/setting.yaml",
					switchEnv, home)
				return
			}

			// Ask for confirmation before deletion
			fmt.Printf("Are you sure you want to delete the environment '%s'? (Y/N): ", removeEnv)
			var response string
			fmt.Scanln(&response)
			response = strings.ToLower(strings.TrimSpace(response))

			if response == "y" {
				// Remove the environment from the environments map
				envMap := targetViper.GetStringMap("environments")
				delete(envMap, removeEnv)
				targetViper.Set("environments", envMap)

				// Write the updated configuration back to the respective setting file
				if err := targetViper.WriteConfig(); err != nil {
					pterm.Error.Printf("Failed to update setting file '%s': %v", targetSettingPath, err)
					return
				}

				// If the deleted environment was the current one, unset it
				if currentEnv == removeEnv {
					appV.Set("environment", "")
					if err := appV.WriteConfig(); err != nil {
						pterm.Error.Printf("Failed to update environment in setting.yaml: %v", err)
						return
					}
					pterm.Info.WithShowLineNumber(false).Println("Cleared current environment in setting.yaml")
				}

				// Display success message
				pterm.Success.Printf("Removed '%s' environment from %s.\n", removeEnv, targetSettingPath)
			} else {
				pterm.Info.Println("Environment deletion canceled.")
			}
			return
		}

		// Check if the -l flag is provided
		listOnly, _ := cmd.Flags().GetBool("list")

		// List environments if the -l flag is set
		if listOnly {
			// Get environment maps from both app and user settings
			appEnvMap := appV.GetStringMap("environments")

			// Map to store all unique environments
			allEnvs := make(map[string]bool)

			// Add app environments
			for envName := range appEnvMap {
				allEnvs[envName] = true
			}

			if len(allEnvs) == 0 {
				pterm.Println("No environments found in setting file")
				return
			}

			pterm.Println("Available Environments:")

			// Print environments with their source and current status
			for envName := range allEnvs {
				if envName == currentEnv {
					pterm.FgGreen.Printf("%s (current)\n", envName)
				} else {
					if _, isApp := appEnvMap[envName]; isApp {
						pterm.Printf("%s\n", envName)
					}
				}
			}
			return
		}

		// If no flags are provided, show help by default
		cmd.Help()
	},
}

// showCmd displays the current cfctl configuration
var showCmd = &cobra.Command{
	Use:   "show",
	Short: "Display the current cfctl configuration",
	Run: func(cmd *cobra.Command, args []string) {
		settingDir := GetSettingDir()
		appSettingPath := filepath.Join(settingDir, "setting.yaml")
		userSettingPath := filepath.Join(settingDir, "cache", "setting.yaml")

		// Create separate Viper instances
		appV := viper.New()
		userV := viper.New()

		// Load app configuration
		if err := loadSetting(appV, appSettingPath); err != nil {
			pterm.Error.Println(err)
			return
		}

		currentEnv := getCurrentEnvironment(appV)
		if currentEnv == "" {
			pterm.Sprintf("No environment set in %s\n", appSettingPath)
			return
		}

		// Try to get the environment from appViper
		envSetting := appV.GetStringMap(fmt.Sprintf("environments.%s", currentEnv))

		// If not found in appViper, try userViper
		if len(envSetting) == 0 {
			envSetting = userV.GetStringMap(fmt.Sprintf("environments.%s", currentEnv))
			if len(envSetting) == 0 {
				pterm.Error.Printf("Environment '%s' not found in %s or %s\n", currentEnv, appSettingPath, userSettingPath)
				return
			}
		}

		output, _ := cmd.Flags().GetString("output")

		switch output {
		case "json":
			data, err := json.MarshalIndent(envSetting, "", "  ")
			if err != nil {
				log.Fatalf("Error formatting output as JSON: %v", err)
			}
			fmt.Println(string(data))
		case "yaml":
			data, err := yaml.Marshal(envSetting)
			if err != nil {
				log.Fatalf("Error formatting output as yaml: %v", err)
			}
			fmt.Println(string(data))
		default:
			log.Fatalf("Unsupported output format: %v", output)
		}
	},
}

// settingEndpointCmd updates the endpoint for the current environment
var settingEndpointCmd = &cobra.Command{
	Use:   "endpoint",
	Short: "Set the endpoint for the current environment",
	Long: `Update the endpoint for the current environment.
You can either specify a new endpoint URL directly or use the service-based endpoint update.`,
	Run: func(cmd *cobra.Command, args []string) {
		urlFlag, _ := cmd.Flags().GetString("url")
		service, _ := cmd.Flags().GetString("service")
		listFlag, _ := cmd.Flags().GetBool("list")

		// Create a new Viper instance for app setting
		appV := viper.New()
		settingPath := filepath.Join(GetSettingDir(), "setting.yaml")
		appV.SetConfigFile(settingPath)
		appV.SetConfigType("yaml")

		if err := loadSetting(appV, settingPath); err != nil {
			pterm.Error.Println(err)
			return
		}

		currentEnv := getCurrentEnvironment(appV)
		if currentEnv == "" {
			pterm.Error.Println("No environment is set. Please initialize or switch to an environment.")
			return
		}

		endpoint, err := getEndpoint(appV)
		if err != nil {
			pterm.Error.Println("Error retrieving endpoint:", err)
			return
		}

		// Get identity service endpoint
		apiEndpoint, err := GetAPIEndpoint(endpoint)
		if err != nil {
			pterm.Error.Printf("Failed to get API endpoint: %v\n", err)
			return
		}

		identityEndpoint, hasIdentityService, err := GetIdentityEndpoint(apiEndpoint)
		if err != nil {
			pterm.Error.Printf("Failed to get identity endpoint: %v\n", err)
			return
		}
		restIdentityEndpoint := apiEndpoint + "/identity"

		// If list flag is provided, only show available services
		if listFlag {
			token, err := getToken(appV)
			if err != nil {
				pterm.Error.Println("Error retrieving token:", err)
				return
			}

			services, err := fetchAvailableServices(endpoint, identityEndpoint, restIdentityEndpoint, hasIdentityService, token)
			if err != nil {
				pterm.Error.Println("Error fetching available services:", err)
				return
			}

			if len(services) == 0 {
				pterm.Println("No available services found.")
				return
			}

			var formattedServices []string
			for _, service := range services {
				if service == "identity" {
					formattedServices = append(formattedServices, pterm.FgCyan.Sprintf("%s (proxy)", service))
				} else {
					formattedServices = append(formattedServices, pterm.FgDefault.Sprint(service))
				}
			}

			pterm.DefaultBox.WithTitle("Available Services").
				WithRightPadding(1).
				WithLeftPadding(1).
				WithTopPadding(0).
				WithBottomPadding(0).
				Println(strings.Join(formattedServices, "\n"))
			return
		}

		if urlFlag == "" && service == "" {
			pterm.DefaultBox.
				WithTitle("Required Flags").
				WithTitleTopCenter().
				WithBoxStyle(pterm.NewStyle(pterm.FgLightBlue)).
				WithRightPadding(1).
				WithLeftPadding(1).
				Println("Please use one of the following flags:")

			pterm.Info.Println("To update endpoint URL directly:")
			pterm.Printf("  $ cfctl setting endpoint -u %s\n\n", pterm.FgLightCyan.Sprint("https://example.com"))

			pterm.Info.Println("To update endpoint based on service:")
			pterm.Printf("  $ cfctl setting endpoint -s %s\n\n", pterm.FgLightCyan.Sprint("identity"))

			cmd.Help()
			return
		}

		if urlFlag != "" {
			// Update endpoint directly with URL
			appV.Set(fmt.Sprintf("environments.%s.endpoint", currentEnv), urlFlag)
			if err := appV.WriteConfig(); err != nil {
				pterm.Error.Printf("Failed to update setting.yaml: %v\n", err)
				return
			}
			pterm.Success.Printf("Updated endpoint for '%s' to '%s'.\n", currentEnv, urlFlag)
			return
		}

		token, err := getToken(appV)
		services, err := fetchAvailableServices(endpoint, identityEndpoint, restIdentityEndpoint, hasIdentityService, token)
		if err != nil {
			pterm.Error.Println("Error fetching available services:", err)
			return
		}

		if len(services) == 0 {
			pterm.Println("No available services found.")
			return
		}

		var formattedServices []string
		for _, service := range services {
			if service == "identity" {
				formattedServices = append(formattedServices, pterm.FgCyan.Sprintf("%s (proxy)", service))
			} else {
				formattedServices = append(formattedServices, pterm.FgDefault.Sprint(service))
			}
		}

		pterm.DefaultBox.WithTitle("Available Services").
			WithRightPadding(1).
			WithLeftPadding(1).
			WithTopPadding(0).
			WithBottomPadding(0).
			Println(strings.Join(formattedServices, "\n"))

		// Create Viper instances for both app and cache settings
		cacheV := viper.New()

		// Load cache configuration
		cachePath := filepath.Join(GetSettingDir(), "cache", "setting.yaml")
		cacheV.SetConfigFile(cachePath)
		cacheV.SetConfigType("yaml")

		if err := loadSetting(appV, settingPath); err != nil {
			pterm.Error.Println(err)
			return
		}

		currentEnv = getCurrentEnvironment(appV)
		if currentEnv == "" {
			pterm.Error.Println("No environment is set. Please initialize or switch to an environment.")
			return
		}

		// Determine prefix from the current environment
		var prefix string
		if strings.HasPrefix(currentEnv, "dev-") {
			prefix = "dev"
		} else if strings.HasPrefix(currentEnv, "stg-") {
			prefix = "stg"
		} else {
			pterm.Error.Printf("Unsupported environment prefix for '%s'.\n", currentEnv)
			return
		}

		// Construct new endpoint
		newEndpoint := fmt.Sprintf("grpc+ssl://%s.api.%s.spaceone.dev:443", service, prefix)

		// Update the appropriate setting file based on environment type
		if strings.HasSuffix(currentEnv, "-app") {
			// Update endpoint in main setting for app environments
			appV.Set(fmt.Sprintf("environments.%s.endpoint", currentEnv), newEndpoint)
			if service != "identity" {
				appV.Set(fmt.Sprintf("environments.%s.proxy", currentEnv), false)
			} else {
				appV.Set(fmt.Sprintf("environments.%s.proxy", currentEnv), true)
			}

			if err := appV.WriteConfig(); err != nil {
				pterm.Error.Printf("Failed to update setting.yaml: %v\n", err)
				return
			}
		} else {
			// Update endpoint in cache setting for user environments
			cachePath := filepath.Join(GetSettingDir(), "cache", "setting.yaml")
			if err := loadSetting(cacheV, cachePath); err != nil {
				pterm.Error.Println(err)
				return
			}

			cacheV.Set(fmt.Sprintf("environments.%s.endpoint", currentEnv), newEndpoint)
			if service != "identity" {
				cacheV.Set(fmt.Sprintf("environments.%s.proxy", currentEnv), false)
			} else {
				cacheV.Set(fmt.Sprintf("environments.%s.proxy", currentEnv), true)
			}

			if err := cacheV.WriteConfig(); err != nil {
				pterm.Error.Printf("Failed to update cache/setting.yaml: %v\n", err)
				return
			}
		}

		pterm.Success.Printf("Updated endpoint for '%s' to '%s'.\n", currentEnv, newEndpoint)
	},
}

// settingTokenCmd updates the token for the current environment
var settingTokenCmd = &cobra.Command{
	Use:   "token [token_value]",
	Short: "Set the token for the current environment",
	Long: `Update the token for the current environment.
This command only works with app environments (-app suffix).`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// Load current environment configuration file
		settingDir := GetSettingDir()
		settingPath := filepath.Join(settingDir, "setting.yaml")

		v := viper.New()
		v.SetConfigFile(settingPath)
		v.SetConfigType("yaml")

		if err := v.ReadInConfig(); err != nil {
			pterm.Error.Printf("Failed to read setting file: %v\n", err)
			return
		}

		// Get current environment
		currentEnv := v.GetString("environment")
		if currentEnv == "" {
			pterm.Error.Println("No environment is currently selected.")
			return
		}

		// Check if it's an app environment
		if !strings.HasSuffix(currentEnv, "-app") {
			pterm.Error.Println("Token can only be set for app environments (-app suffix).")
			return
		}

		// Update token
		tokenKey := fmt.Sprintf("environments.%s.token", currentEnv)
		v.Set(tokenKey, args[0])

		// Save configuration
		if err := v.WriteConfig(); err != nil {
			pterm.Error.Printf("Failed to update token: %v\n", err)
			return
		}

		pterm.Success.Printf("Token updated for '%s' environment.\n", currentEnv)
	},
}

// fetchAvailableServices retrieves the list of services by calling the List method on the Endpoint service.
func fetchAvailableServices(endpoint, identityEndpoint, restIdentityEndpoint string, hasIdentityEndpoint bool, token string) ([]string, error) {
	if !hasIdentityEndpoint {
		// Create HTTP client and request
		client := &http.Client{}

		// Define response structure
		type EndpointResponse struct {
			Results []struct {
				Service string `json:"service"`
			} `json:"results"`
		}

		// Create and send request
		req, err := http.NewRequest("POST", restIdentityEndpoint+"/endpoint/list", bytes.NewBuffer([]byte("{}")))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %v", err)
		}

		req.Header.Set("accept", "application/json")
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to send request: %v", err)
		}
		defer resp.Body.Close()

		// Parse response
		var response EndpointResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return nil, fmt.Errorf("failed to decode response: %v", err)
		}

		// Extract services
		var availableServices []string
		for _, result := range response.Results {
			availableServices = append(availableServices, result.Service)
		}

		return availableServices, nil
	} else {
		parsedURL, err := url.Parse(identityEndpoint)
		if err != nil {
			return nil, fmt.Errorf("failed to parse endpoint: %w", err)
		}

		host := parsedURL.Hostname()
		port := parsedURL.Port()
		if port == "" {
			port = "443" // Default gRPC port
		}

		var opts []grpc.DialOption

		// Set up TLS credentials if the scheme is grpc+ssl://
		if strings.HasPrefix(identityEndpoint, "grpc+ssl://") {
			tlsSetting := &tls.Config{
				InsecureSkipVerify: false, // Set to true only if you want to skip TLS verification (not recommended)
			}
			creds := credentials.NewTLS(tlsSetting)
			opts = append(opts, grpc.WithTransportCredentials(creds))
		} else {
			return nil, fmt.Errorf("unsupported scheme in endpoint: %s", identityEndpoint)
		}

		// Add token-based authentication if a token is provided
		if token != "" {
			opts = append(opts, grpc.WithPerRPCCredentials(&tokenCreds{token}))
		}

		// Establish a connection to the gRPC server
		conn, err := grpc.Dial(fmt.Sprintf("%s:%s", host, port), opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to dial gRPC endpoint: %w", err)
		}
		defer conn.Close()

		ctx := context.Background()

		// Create a reflection client to discover services and methods
		refClient := grpcreflect.NewClient(ctx, grpc_reflection_v1alpha.NewServerReflectionClient(conn))
		defer refClient.Reset()

		// Resolve the service descriptor for "spaceone.api.identity.v2.Endpoint"
		serviceName := "spaceone.api.identity.v2.Endpoint"
		svcDesc, err := refClient.ResolveService(serviceName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve service %s: %w", serviceName, err)
		}

		// Resolve the method descriptor for the "List" method
		methodName := "list"
		methodDesc := svcDesc.FindMethodByName(methodName)
		if methodDesc == nil {
			return nil, fmt.Errorf("method '%s' not found in service '%s'", methodName, serviceName)
		}

		inputType := methodDesc.GetInputType()
		if inputType == nil {
			return nil, fmt.Errorf("input type not found for method '%s'", methodName)
		}

		// Get the request and response message descriptors
		reqDesc := methodDesc.GetInputType()
		respDesc := methodDesc.GetOutputType()

		// Create a dynamic message for the request
		reqMsg := dynamic.NewMessage(reqDesc)
		// If ListRequest has required fields, set them here. For example:
		// reqMsg.SetField("page_size", 100)

		// Create a dynamic message for the response
		respMsg := dynamic.NewMessage(respDesc)

		// Invoke the RPC method
		//err = grpc.Invoke(ctx, fmt.Sprintf("/%s/%s", serviceName, methodName), reqMsg, conn, respMsg)
		err = conn.Invoke(ctx, fmt.Sprintf("/%s/%s", serviceName, methodName), reqMsg, respMsg)
		if err != nil {
			return nil, fmt.Errorf("failed to invoke RPC: %w", err)
		}

		// Extract the 'results' field from the response message
		resultsFieldDesc := respDesc.FindFieldByName("results")
		if resultsFieldDesc == nil {
			return nil, fmt.Errorf("'results' field not found in response message")
		}

		resultsField, err := respMsg.TryGetField(resultsFieldDesc)
		if err != nil {
			return nil, fmt.Errorf("failed to get 'results' field: %w", err)
		}

		// 'results' is expected to be a repeated field (list) of messages
		resultsSlice, ok := resultsField.([]interface{})
		if !ok {
			return nil, fmt.Errorf("'results' field is not a list")
		}

		var availableServices []string
		for _, res := range resultsSlice {
			// Each item in 'results' should be a dynamic.Message
			resMsg, ok := res.(*dynamic.Message)
			if !ok {
				continue
			}

			// Extract the 'service' field from each result message
			serviceFieldDesc := resMsg.GetMessageDescriptor().FindFieldByName("service")
			if serviceFieldDesc == nil {
				continue // Skip if 'service' field is not found
			}

			serviceField, err := resMsg.TryGetField(serviceFieldDesc)
			if err != nil {
				continue // Skip if unable to get the 'service' field
			}

			serviceStr, ok := serviceField.(string)
			if !ok {
				continue // Skip if 'service' field is not a string
			}

			availableServices = append(availableServices, serviceStr)
		}

		return availableServices, nil
	}
}

// tokenCreds implements grpc.PerRPCCredentials for token-based authentication.
type tokenCreds struct {
	token string
}

func (t *tokenCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": fmt.Sprintf("Bearer %s", t.token),
	}, nil
}

func (t *tokenCreds) RequireTransportSecurity() bool {
	return true
}

// getBaseURL retrieves the base URL for the current environment from the given Viper instance.
func getEndpoint(v *viper.Viper) (string, error) {
	currentEnv := getCurrentEnvironment(v)
	if currentEnv == "" {
		return "", fmt.Errorf("no environment is set")
	}

	baseURL := v.GetString(fmt.Sprintf("environments.%s.endpoint", currentEnv))

	if baseURL == "" {
		return "", fmt.Errorf("no endpoint found for environment '%s' in setting.yaml", currentEnv)

	}

	return baseURL, nil
}

// getToken retrieves the token for the current environment.
func getToken(v *viper.Viper) (string, error) {
	currentEnv := getCurrentEnvironment(v)
	if currentEnv == "" {
		return "", fmt.Errorf("no environment selected")
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %v", err)
	}

	tokenPath := filepath.Join(homeDir, ".cfctl", "cache", currentEnv, "access_token")
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", fmt.Errorf("failed to read token: %v", err)
	}

	return strings.TrimSpace(string(tokenBytes)), nil
}

// GetSettingDir returns the directory where setting file are stored
func GetSettingDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Unable to find home directory: %v", err)
	}
	return filepath.Join(home, ".cfctl")
}

// loadSetting ensures that the setting directory and setting file exist.
// It initializes the setting file with default values if it does not exist.
func loadSetting(v *viper.Viper, settingPath string) error {
	// Ensure the setting directory exists
	settingDir := filepath.Dir(settingPath)
	if err := os.MkdirAll(settingDir, 0755); err != nil {
		return fmt.Errorf("failed to create setting directory '%s': %w", settingDir, err)
	}

	// Set the setting file
	v.SetConfigFile(settingPath)
	v.SetConfigType("yaml")

	// Read the setting file
	if err := v.ReadInConfig(); err != nil {
		if os.IsNotExist(err) {
			// Initialize with default values if file doesn't exist
			defaultSettings := map[string]interface{}{
				"environments": map[string]interface{}{},
				"environment":  "",
			}

			// Write the default settings to file
			if err := v.MergeConfigMap(defaultSettings); err != nil {
				return fmt.Errorf("failed to merge default settings: %w", err)
			}

			if err := v.WriteConfig(); err != nil {
				return fmt.Errorf("failed to write default settings: %w", err)
			}

			// Read the newly created file
			if err := v.ReadInConfig(); err != nil {
				return fmt.Errorf("failed to read newly created setting file: %w", err)
			}
		} else {
			return fmt.Errorf("failed to read setting file: %w", err)
		}
	}

	return nil
}

// getCurrentEnvironment reads the current environment from the given Viper instance
func getCurrentEnvironment(v *viper.Viper) string {
	return v.GetString("environment")
}

// updateGlobalSetting prints a success message for global setting update
func updateGlobalSetting() {
	settingPath := filepath.Join(GetSettingDir(), "setting.yaml")
	v := viper.New()

	v.SetConfigFile(settingPath)

	if err := v.ReadInConfig(); err != nil {
		if os.IsNotExist(err) {
			pterm.Success.WithShowLineNumber(false).Printfln("Global setting updated with existing environments. (default: %s/setting.yaml)", GetSettingDir())
			return
		}
		pterm.Warning.Printf("Warning: Could not read global setting: %v\n", err)
		return
	}

	pterm.Success.WithShowLineNumber(false).Printfln("Global setting updated with existing environments. (default: %s/setting.yaml)", GetSettingDir())
}

func parseEnvNameFromURL(urlStr string) (string, error) {
	urlStr = strings.TrimPrefix(urlStr, "https://")
	urlStr = strings.TrimPrefix(urlStr, "http://")
	urlStr = strings.TrimPrefix(urlStr, "grpc://")
	urlStr = strings.TrimPrefix(urlStr, "grpc+ssl://")

	if strings.Contains(urlStr, "localhost") {
		return "local", nil
	}

	hostParts := strings.Split(urlStr, ":")
	hostname := hostParts[0]

	parts := strings.Split(hostname, ".")

	if isIPAddress(hostname) {
		return "local", nil
	}

	if len(parts) > 0 {
		envName := parts[0]
		reg := regexp.MustCompile(`[^a-zA-Z0-9]+`)
		envName = reg.ReplaceAllString(envName, "")
		return strings.ToLower(envName), nil
	}

	return "", fmt.Errorf("could not determine environment name from URL: %s", urlStr)
}

func isIPAddress(host string) bool {
	ipv4Pattern := `^(\d{1,3}\.){3}\d{1,3}$`
	match, _ := regexp.MatchString(ipv4Pattern, host)
	return match
}

// updateSetting updates the configuration files
func updateSetting(envName, endpoint string, envSuffix string) {
	settingDir := GetSettingDir()
	mainSettingPath := filepath.Join(settingDir, "setting.yaml")

	v := viper.New()
	v.SetConfigFile(mainSettingPath)
	v.SetConfigType("yaml")

	// Create full environment name
	var fullEnvName string
	if envSuffix == "" {
		fullEnvName = envName
	} else {
		fullEnvName = fmt.Sprintf("%s-%s", envName, envSuffix)
	}

	// Read existing config if it exists
	_ = v.ReadInConfig()

	// Set environment
	v.Set("environment", fullEnvName)

	// Handle protocol for endpoint
	if !strings.Contains(endpoint, "://") {
		if strings.Contains(endpoint, "localhost") || strings.Contains(endpoint, "127.0.0.1") {
			endpoint = "http://" + endpoint
		} else {
			endpoint = "https://" + endpoint
		}
	}

	// Set endpoint in environments map
	envKey := fmt.Sprintf("environments.%s.endpoint", fullEnvName)
	v.Set(envKey, endpoint)

	// Set additional configurations based on environment type
	if envName == "local" {
		// Local environment settings (only for pure 'local', not 'local-user' or 'local-app')
		if envSuffix == "" {
			tokenKey := fmt.Sprintf("environments.%s.token", fullEnvName)
			v.Set(tokenKey, "no_token")
		} else {
			// Set proxy for local-user and local-app
			proxyKey := fmt.Sprintf("environments.%s.proxy", fullEnvName)
			v.Set(proxyKey, true)

			// Set token only for app environment
			if envSuffix == "app" {
				tokenKey := fmt.Sprintf("environments.%s.token", fullEnvName)
				v.Set(tokenKey, "no_token")
			}
		}
	} else {
		// Non-local environment settings
		proxyKey := fmt.Sprintf("environments.%s.proxy", fullEnvName)
		v.Set(proxyKey, true)

		// Only set token for app environment
		if envSuffix == "app" {
			tokenKey := fmt.Sprintf("environments.%s.token", fullEnvName)
			v.Set(tokenKey, "no_token")
		}
	}

	if err := v.WriteConfig(); err != nil {
		pterm.Error.Printf("Failed to write setting file: %v\n", err)
		return
	}

	pterm.Success.Printf("Environment '%s' successfully initialized.\n", fullEnvName)
}

// convertToStringMap converts map[interface{}]interface{} to map[string]interface{}
func convertToStringMap(m map[interface{}]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range m {
		switch v := v.(type) {
		case map[interface{}]interface{}:
			result[k.(string)] = convertToStringMap(v)
		case []interface{}:
			result[k.(string)] = convertToSlice(v)
		default:
			result[k.(string)] = v
		}
	}
	return result
}

// convertToSlice handles slice conversion if needed
func convertToSlice(s []interface{}) []interface{} {
	result := make([]interface{}, len(s))
	for i, v := range s {
		switch v := v.(type) {
		case map[interface{}]interface{}:
			result[i] = convertToStringMap(v)
		case []interface{}:
			result[i] = convertToSlice(v)
		default:
			result[i] = v
		}
	}
	return result
}

// constructEndpoint generates the gRPC endpoint string from baseURL
func constructEndpoint(baseURL string) (string, error) {
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	hostname := parsedURL.Hostname()

	switch {
	case strings.Contains(hostname, ".dev.spaceone.dev"):
		return fmt.Sprintf("grpc+ssl://identity.api.dev.spaceone.dev:443"), nil
	case strings.Contains(hostname, ".stg.spaceone.dev"):
		return fmt.Sprintf("grpc+ssl://identity.api.stg.spaceone.dev:443"), nil
	}

	if strings.Contains(hostname, "spaceone.megazone.io") {
		region := "kr1"
		if strings.Contains(hostname, "jp1.") {
			region = "jp1"
		} else if strings.Contains(hostname, "us1.") {
			region = "us1"
		}

		return fmt.Sprintf("https://console-v2.%s.api.spaceone.megazone.io/identity", region), nil
	}

	return "", fmt.Errorf("unknown environment in URL: %s", hostname)
}

func init() {
	SettingCmd.AddCommand(settingInitCmd)
	SettingCmd.AddCommand(envCmd)
	SettingCmd.AddCommand(settingEndpointCmd)
	SettingCmd.AddCommand(showCmd)
	settingInitCmd.AddCommand(settingInitEndpointCmd)
	settingInitCmd.AddCommand(settingInitLocalCmd)

	settingInitEndpointCmd.Flags().Bool("app", false, "Initialize as application configuration")
	settingInitEndpointCmd.Flags().Bool("user", false, "Initialize as user-specific configuration")

	envCmd.Flags().StringP("switch", "s", "", "Switch to a different environment")
	envCmd.Flags().StringP("remove", "r", "", "Remove an environment")
	envCmd.Flags().BoolP("list", "l", false, "List available environments")

	showCmd.Flags().StringP("output", "o", "yaml", "Output format (yaml/json)")

	settingEndpointCmd.Flags().StringP("url", "u", "", "Direct URL to set as endpoint")
	settingEndpointCmd.Flags().StringP("service", "s", "", "Service to set the endpoint for")
	settingEndpointCmd.Flags().BoolP("list", "l", false, "List available services")
}
