package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
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

// Global variables
var (
	formRegistry  map[string]FormData
	conversations = make(map[string]*ConversationState)
	chatClient    *openai.Client
)

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

		log.Printf("Initializing new conversation for form %s with system prompt: %s", formID, form.SystemPrompt)
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

	// Log final conversation state
	log.Printf("Final conversation state:")
	for i, msg := range conv.Messages {
		log.Printf("Message %d - Role: %s, Content: %s", i, msg.Role, msg.Content)
	}

	// Send the AI's response to the user
	aiResponse := ChatResponse{Response: aiMessage.Content}
	responseDump, err := json.MarshalIndent(aiResponse, "", "  ")
	if err != nil {
		log.Printf("Error marshaling response: %v", err)
	} else {
		log.Printf("Sending response to user:\n%s", string(responseDump))
	}
	json.NewEncoder(w).Encode(aiResponse)
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
