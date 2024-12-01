package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

type ConfigurationForm struct {
	Name   string `xml:"name,attr"`
	Config string `xml:"config"`
	Fields string `xml:"form_fields"`
	Prompt string `xml:"system_prompt"`
}

// Configuration structures
type Configuration struct {
	Model        string   `xml:"model"`
	SystemPrompt string   `xml:"system_prompt"`
	XMLName      xml.Name `xml:"configuration"`
	SiteTitle    string   `xml:"site_title"`
	BaseURL      string   `xml:"base_url"`
	Templates    struct {
		Template []struct {
			Name string `xml:"name,attr"`
			HTML string `xml:",chardata"`
		} `xml:"template"`
	} `xml:"templates"`
	Forms struct {
		Form []ConfigurationForm `xml:"form"`
	} `xml:"forms"`
}

func (c Configuration) FormByName(formName string) ConfigurationForm {
	for _, form := range c.Forms.Form {
		if form.Name == formName {
			return form
		}
	}
	panic(fmt.Sprintf("Form %s not found in configuration", formName))
}

// Chat structures
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type ChatSession struct {
	Messages []ChatMessage
	FormData map[string]string
}

// Global session storage
var chatSessions = make(map[string]*ChatSession)

type FormField struct {
	Label   string
	Name    string
	Example string
}

func parseFormFields(fieldsStr string) []FormField {
	lines := strings.Split(fieldsStr, "\n")
	fields := make([]FormField, 0)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse "Field Label: {{.FieldName}} (like example)"
		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			continue
		}

		label := strings.TrimSpace(parts[0])

		// Extract field name from {{.FieldName}}
		nameMatch := regexp.MustCompile(`{{\.(\w+)}}`).FindStringSubmatch(parts[1])
		if len(nameMatch) < 2 {
			continue
		}
		name := nameMatch[1]

		// Extract example if present
		example := ""
		if idx := strings.Index(parts[1], "(like "); idx != -1 {
			example = strings.Trim(parts[1][idx+6:], ")")
		}

		fields = append(fields, FormField{
			Label:   label,
			Name:    name,
			Example: example,
		})
	}

	return fields
}

func main() {
	// Load configuration
	data, err := os.ReadFile("configuration.xml")
	if err != nil {
		log.Fatalf("Error reading config: %v", err)
	}

	var config Configuration
	if err := xml.Unmarshal(data, &config); err != nil {
		log.Fatalf("Error parsing config: %v", err)
	}

	// Home page handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		// Find home template
		var homeHTML string
		for _, tmpl := range config.Templates.Template {
			if tmpl.Name == "home_page" {
				homeHTML = tmpl.HTML
				break
			}
		}

		tmpl := template.Must(template.New("home").Parse(homeHTML))
		tmpl.Execute(w, config)
	})

	// QR code handler
	http.HandleFunc("/qr/", func(w http.ResponseWriter, r *http.Request) {
		formPath := strings.TrimPrefix(r.URL.Path, "/qr/")
		formURL := config.BaseURL + "/form/" + formPath

		png, err := qrcode.Encode(formURL, qrcode.Medium, 256)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	})

	// Set up form handlers
	for _, form := range config.Forms.Form {
		formName := form.Name
		formPath := "/form/" + formName

		log.Printf("Setting up handlers for form: %s at path: %s", formName, formPath)

		// Form page handler
		http.HandleFunc(formPath, func(w http.ResponseWriter, r *http.Request) {
			var formHTML string
			for _, tmpl := range config.Templates.Template {
				if tmpl.Name == "chat_form" {
					formHTML = tmpl.HTML
					break
				}
			}

			// Parse form fields and log them
			fields := parseFormFields(form.Fields)
			log.Printf("Parsed fields: %+v", fields)

			// Try to load existing data
			initialData := make(map[string]string)
			if license := r.URL.Query().Get("License"); license != "" {
				filename := fmt.Sprintf("forms/registration-%s.json", license)
				if data, err := os.ReadFile(filename); err == nil {
					json.Unmarshal(data, &initialData)
				}
			}

			data := map[string]interface{}{
				"Fields":      fields,
				"InitialData": initialData,
			}
			log.Printf("Template data: %+v", data)

			tmpl := template.Must(template.New("form").Parse(formHTML))
			if err := tmpl.Execute(w, data); err != nil {
				log.Printf("Template error: %v", err)
			}
		})

		// Chat endpoint
		http.HandleFunc(formPath+"/chat", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			handleChat(w, r, config, formName)
		})
	}

	log.Printf("Server starting on %s", config.BaseURL)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleChat(w http.ResponseWriter, r *http.Request, config Configuration, formName string) {
	log.Printf("=== Chat request received for form: %s ===", formName)

	var chatReq struct {
		Message  string `json:"message"`
		AutoSave bool   `json:"autoSave"`
	}
	if err := json.NewDecoder(r.Body).Decode(&chatReq); err != nil {
		log.Printf("ERROR [%s]: Failed to decode chat request: %v", formName, err)
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	log.Printf("👤 USER [%s]: %s", formName, chatReq.Message)

	// Get or create session
	session := chatSessions[formName]
	if session == nil {
		log.Printf("📝 Creating new chat session for form: %s", formName)
		session = &ChatSession{
			Messages: []ChatMessage{
				{
					Role: "system",
					Content: fmt.Sprintf(
						// expects parameters: global prompt, form fields, registration data
						config.FormByName(formName).Prompt,
						config.SystemPrompt,
						config.FormByName(formName).Fields,
						"", // ie: for visit form, registration data goes here
					),
				},
			},
			FormData: make(map[string]string),
		}
		chatSessions[formName] = session
	}

	// If it's an auto-save request, just handle the SET and save
	if chatReq.AutoSave {
		if strings.HasPrefix(chatReq.Message, "SET ") {
			parts := strings.SplitN(strings.TrimPrefix(chatReq.Message, "SET "), " ", 2)
			if len(parts) == 2 {
				field := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				session.FormData[field] = value

				// Save immediately
				filename := fmt.Sprintf("forms/registration-%s.json", session.FormData["License"])
				formJSON, _ := json.MarshalIndent(session.FormData, "", "    ")
				os.MkdirAll("forms", 0755)
				os.WriteFile(filename, formJSON, 0644)

				json.NewEncoder(w).Encode(map[string]interface{}{
					"updates": map[string]string{field: value},
				})
				return
			}
		}
	}

	// Add user message to history
	session.Messages = append(session.Messages, ChatMessage{
		Role:    "user",
		Content: chatReq.Message,
	})

	// Call ChatGPT
	resp, err := callChatGPT(config, session.Messages)
	if err != nil {
		log.Printf("❌ ERROR [%s]: ChatGPT error: %v", formName, err)
		http.Error(w, "AI service error", http.StatusInternalServerError)
		return
	}

	if len(resp.Choices) > 0 {
		aiMessage := resp.Choices[0].Message
		log.Printf("🤖 AI [%s]: \"%s\"", formName, aiMessage.Content)

		// Parse and log commands from AI response
		lines := strings.Split(aiMessage.Content, "\n")
		var responseText string
		formUpdates := make(map[string]string)
		var shouldSave bool

		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			switch {
			case strings.HasPrefix(line, "SET "):
				parts := strings.SplitN(strings.TrimPrefix(line, "SET "), " ", 2)
				if len(parts) == 2 {
					field := strings.TrimSpace(parts[0])
					value := strings.TrimSpace(parts[1])
					formUpdates[field] = value
					session.FormData[field] = value
				}
			case strings.HasPrefix(line, "SAY "):
				text := strings.TrimSpace(strings.TrimPrefix(line, "SAY "))
				if responseText == "" {
					responseText = text
				}
				log.Printf("💬 [%s]: \"%s\"", formName, line)
			case line == "SAVE":
				shouldSave = true
				log.Printf("💾 [%s]: \"%s\"", formName, line)
			}
		}

		// Handle form saving
		if shouldSave {
			filename := fmt.Sprintf("forms/registration-%s.json", session.FormData["License"])
			log.Printf("💾 SAVE [%s]: Saving to %s", formName, filename)

			// Change this part to save the actual form data
			formJSON, err := json.MarshalIndent(session.FormData, "", "    ")
			if err != nil {
				log.Printf("❌ ERROR [%s]: Failed to marshal form data: %v", formName, err)
				http.Error(w, "Failed to save form", http.StatusInternalServerError)
				return
			}

			if err := os.MkdirAll("forms", 0755); err != nil {
				log.Printf("❌ ERROR [%s]: Failed to create forms directory: %v", formName, err)
				http.Error(w, "Failed to create forms directory", http.StatusInternalServerError)
				return
			}

			if err := os.WriteFile(filename, formJSON, 0644); err != nil {
				log.Printf("❌ ERROR [%s]: Failed to write to %s: %v", formName, filename, err)
				http.Error(w, "Failed to save form", http.StatusInternalServerError)
				return
			}
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": responseText,
			"updates": formUpdates,
		})
	}
}

func callChatGPT(config Configuration, messages []ChatMessage) (*ChatResponse, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY environment variable not set")
	}

	requestBody, err := json.Marshal(map[string]interface{}{
		"model":    config.Model,
		"messages": messages,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, err
	}

	return &chatResp, nil
}
