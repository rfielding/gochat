package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	openai "github.com/sashabaranov/go-openai"
	qrcode "github.com/skip2/go-qrcode"
)

// FormData defines the structure of a form configuration
type FormData struct {
	Path             string `json:"path"`
	Title            string `json:"title"`
	Description      string `json:"description"`
	ButtonText       string `json:"button_text"`
	PrimaryKey       string `json:"primary_key"`
	PrerequisiteForm string `json:"prerequisite_form"`
	NextForm         string `json:"next_form"`
	FormTemplate     string `json:"form_template"`
	SystemPrompt     string `json:"system_prompt"`
	FormFields       string `json:"form_fields"`
}

// Template defines the structure for HTML templates
type Template struct {
	HTML string `json:"html"`
}

// SiteConfig defines the overall site configuration
type SiteConfig struct {
	SiteTitle string              `json:"site_title"`
	BaseURL   string              `json:"base_url"`
	Templates map[string]Template `json:"templates"`
	Forms     map[string]FormData `json:"forms"`
}

// ConversationState tracks the chat history
type ConversationState struct {
	Messages []openai.ChatCompletionMessage
}

// ChatRequest defines the structure for incoming chat messages
type ChatRequest struct {
	Message string `json:"message"`
}

// ChatResponse defines the structure for outgoing chat messages
type ChatResponse struct {
	Response string `json:"response"`
}

// Add this struct to store the extracted form data
type RegistrationData struct {
	FullName    string `json:"full_name"`
	DateOfBirth string `json:"date_of_birth"`
	License     string `json:"license"`
	Contact     string `json:"contact"`
}

// Global variables
var (
	formRegistry  map[string]FormData
	conversations = make(map[string]*ConversationState)
	chatClient    *openai.Client
)

type Configuration struct {
	XMLName   xml.Name `xml:"configuration"`
	SiteTitle string   `xml:"site_title"`
	BaseURL   string   `xml:"base_url"`
	Templates struct {
		Template []struct {
			Name string `xml:"name,attr"`
			HTML string `xml:",chardata"`
		} `xml:"template"`
	} `xml:"templates"`
	Forms struct {
		Form []struct {
			Name   string `xml:"name,attr"`
			Config string `xml:"config"`
			Fields string `xml:"form_fields"`
			Prompt string `xml:"system_prompt"`
		} `xml:"form"`
	} `xml:"forms"`
}

func main() {
	// Initialize OpenAI client with your API key
	chatClient = openai.NewClient(os.Getenv("OPENAI_API_KEY"))

	// Load the configuration from JSON
	data, err := os.ReadFile("configuration.json")
	if err != nil {
		log.Fatalf("Failed to read configuration: %v", err)
	}

	var config SiteConfig
	if err := json.Unmarshal(data, &config); err != nil {
		log.Fatalf("Failed to parse configuration: %v", err)
	}

	formRegistry = config.Forms

	// Set up HTTP handlers for each form
	for formID, form := range formRegistry {
		formID := formID // Create new variable to avoid closure issues
		form := form     // Create new variable to avoid closure issues

		http.HandleFunc(form.Path, func(w http.ResponseWriter, r *http.Request) {
			// Add content type header
			w.Header().Set("Content-Type", "text/html; charset=utf-8")

			// Debug logging
			log.Printf("Serving form template: %s using template: %s", formID, form.FormTemplate)

			tmpl := template.Must(template.New(formID).Parse(config.Templates[form.FormTemplate].HTML))
			err := tmpl.Execute(w, form)
			if err != nil {
				log.Printf("Template execution error: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		})

		http.HandleFunc(form.Path+"/chat", func(w http.ResponseWriter, r *http.Request) {
			// Only accept POST requests for chat
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}

			handleChat(w, r, formID)
		})
	}

	// Add home page handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		tmpl := template.Must(template.New("home").Parse(config.Templates["home_page"].HTML))
		err := tmpl.Execute(w, config)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})

	// Add QR code handler
	http.HandleFunc("/qr/", func(w http.ResponseWriter, r *http.Request) {
		formID := strings.TrimPrefix(r.URL.Path, "/qr/")
		form, exists := formRegistry[formID]
		if !exists {
			http.NotFound(w, r)
			return
		}

		formURL := config.BaseURL + form.Path
		qr, err := qrcode.New(formURL, qrcode.Medium)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "image/png")
		qr.Write(256, w)
	})

	// Start the server
	log.Printf("Starting server on %s", config.BaseURL)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleChat(w http.ResponseWriter, r *http.Request, formID string) {
	// Log incoming request
	requestDump, err := httputil.DumpRequest(r, true)
	if err != nil {
		log.Printf("Error dumping request: %v", err)
	} else {
		log.Printf("Incoming request:\n%s", string(requestDump))
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Get or initialize conversation
	conv, exists := conversations[formID]
	if !exists {
		form, ok := formRegistry[formID]
		if !ok {
			http.Error(w, "Unknown form type", http.StatusBadRequest)
			return
		}

		// For visit form, check for registration data
		if formID == "visit" {
			// Try to get license from query parameter
			license := r.URL.Query().Get("key")
			if license != "" {
				// Load registration data
				regData, err := loadFormData("register", license)
				if err == nil {
					// Update system prompt with registration data
					form.SystemPrompt = strings.Replace(
						form.SystemPrompt,
						"{{.registration_data}}",
						fmt.Sprintf("Name: %s, License: %s", regData["full_name"], license),
						1,
					)
				}
			} else {
				// Modify system prompt to ask for license first
				form.SystemPrompt = "Please ask the user for their license number first. Once provided, we can proceed with the visit form."
			}
		}

		// Initialize conversation with appropriate system prompt
		conv = &ConversationState{
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: form.SystemPrompt,
				},
			},
		}
		conversations[formID] = conv
	}

	// Log current conversation state
	log.Printf("Current conversation state before adding new message:")
	for i, msg := range conv.Messages {
		log.Printf("Message %d - Role: %s, Content: %s", i, msg.Role, msg.Content)
	}

	// Add user message to conversation
	conv.Messages = append(conv.Messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: req.Message,
	})

	log.Printf("Sending request to ChatGPT with %d messages", len(conv.Messages))

	// Send to ChatGPT to get its response
	response, err := chatClient.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model:    "gpt-3.5-turbo",
			Messages: conv.Messages,
		},
	)
	if err != nil {
		log.Printf("Error from ChatGPT: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Log ChatGPT's response
	log.Printf("ChatGPT response: %+v", response)

	// Add AI's response to conversation
	aiMessage := response.Choices[0].Message
	conv.Messages = append(conv.Messages, aiMessage)

	// Debug logging for conversation state
	log.Printf("Current conversation state:")
	for i, msg := range conv.Messages {
		log.Printf("Message %d - Role: %s, Content: %s", i, msg.Role, msg.Content)
	}

	// Check if this is a confirmation "yes" after the summary
	if isFormComplete(conv.Messages) {
		log.Printf("Form completion detected!")
		form := formRegistry[formID]
		data, err := extractFormData(conv.Messages)
		if err != nil {
			log.Printf("Error extracting form data: %v", err)
			http.Error(w, "Error processing form data", http.StatusInternalServerError)
			return
		}

		// Save the form data
		filename := fmt.Sprintf("forms/%s-%s.json", data.License, formID)
		if err := saveFormData(filename, data); err != nil {
			log.Printf("Error saving form data: %v", err)
			http.Error(w, "Error saving form data", http.StatusInternalServerError)
			return
		}

		// Generate next form URL with absolute path
		if form.NextForm != "" {
			nextForm := formRegistry[form.NextForm]
			nextFormURL := fmt.Sprintf("%s?key=%s", nextForm.Path, data.License)
			aiMessage.Content += fmt.Sprintf("\n\nClick here to continue: %s", nextFormURL)
			log.Printf("Generated next form URL: %s", nextFormURL)
		}
	}

	// Send the AI's response to the user
	aiResponse := ChatResponse{Response: aiMessage.Content}
	json.NewEncoder(w).Encode(aiResponse)
}

func isFormComplete(messages []openai.ChatCompletionMessage) bool {
	if len(messages) < 1 {
		return false
	}

	// Check if the last AI message contains the trigger phrase
	lastMsg := messages[len(messages)-1].Content
	log.Printf("Checking last message: %s", lastMsg)

	return strings.Contains(lastMsg, "We are saving this form in our database.")
}

func extractFormData(messages []openai.ChatCompletionMessage) (*RegistrationData, error) {
	// Find the summary message by looking for all required fields
	var summaryMsg string
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i].Content
		if strings.Contains(msg, "Full Name:") &&
			strings.Contains(msg, "Date of Birth:") &&
			strings.Contains(msg, "License Number:") &&
			strings.Contains(msg, "Contact Information:") {
			summaryMsg = msg
			break
		}
	}

	if summaryMsg == "" {
		return nil, fmt.Errorf("no summary found in conversation")
	}

	log.Printf("Found summary message: %s", summaryMsg)

	// Parse the summary using regex
	data := &RegistrationData{}

	// Extract full name (now handling bullet points or dashes)
	if match := regexp.MustCompile(`Full Name: (.*?)[\n$]`).FindStringSubmatch(summaryMsg); len(match) > 1 {
		data.FullName = strings.TrimSpace(match[1])
	}
	// Extract DOB
	if match := regexp.MustCompile(`Date of Birth: (.*?)[\n$]`).FindStringSubmatch(summaryMsg); len(match) > 1 {
		data.DateOfBirth = strings.TrimSpace(match[1])
	}
	// Extract license
	if match := regexp.MustCompile(`License Number: (.*?)[\n$]`).FindStringSubmatch(summaryMsg); len(match) > 1 {
		data.License = strings.TrimSpace(match[1])
	}
	// Extract contact
	if match := regexp.MustCompile(`Contact Information: (.*?)[\n$]`).FindStringSubmatch(summaryMsg); len(match) > 1 {
		data.Contact = strings.TrimSpace(match[1])
	}

	if data.License == "" {
		return nil, fmt.Errorf("could not extract license number from summary")
	}

	log.Printf("Extracted data: %+v", data)
	return data, nil
}

func saveFormData(filename string, data *RegistrationData) error {
	// Ensure the forms directory exists
	if err := os.MkdirAll("forms", 0755); err != nil {
		return fmt.Errorf("failed to create forms directory: %v", err)
	}

	// Marshal the data to JSON
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal form data: %v", err)
	}

	// Write the file
	if err := os.WriteFile(filename, jsonData, 0644); err != nil {
		return fmt.Errorf("failed to write form data: %v", err)
	}

	return nil
}

// Helper function to load form data
func loadFormData(formType string, key string) (map[string]string, error) {
	filename := filepath.Join("forms", fmt.Sprintf("%s-%s.json", key, formType))
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("no form data found for %s: %s", formType, key)
	}

	var formData map[string]string
	if err := json.Unmarshal(data, &formData); err != nil {
		return nil, fmt.Errorf("error parsing form data: %v", err)
	}

	return formData, nil
}

func loadConfiguration(filename string) (*Configuration, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	var config Configuration
	if err := xml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("error parsing configuration: %v", err)
	}

	// Convert to the format expected by the rest of the application
	formRegistry = make(map[string]FormData)
	for _, form := range config.Forms.Form {
		var formData FormData
		if err := json.Unmarshal([]byte(form.Config), &formData); err != nil {
			return nil, fmt.Errorf("error parsing form config: %v", err)
		}
		formData.FormFields = form.Fields
		formData.SystemPrompt = form.Prompt
		formRegistry[form.Name] = formData
	}

	return &config, nil
}

func initializeConversation(form FormData, regData map[string]string) (*ConversationState, error) {
	// For visit form, include registration data in prompt
	var systemPrompt string
	if form.PrerequisiteForm != "" && regData != nil {
		systemPrompt = fmt.Sprintf(form.SystemPrompt,
			formatRegistrationData(regData),
			form.FormFields)
	} else {
		systemPrompt = fmt.Sprintf(form.SystemPrompt, form.FormFields)
	}

	return &ConversationState{
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: systemPrompt,
			},
		},
	}, nil
}

func formatRegistrationData(regData map[string]string) string {
	var result []string
	for key, value := range regData {
		result = append(result, fmt.Sprintf("%s: %s", key, value))
	}
	return strings.Join(result, "\n")
}
