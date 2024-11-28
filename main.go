package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
	qrcode "github.com/skip2/go-qrcode"
)

type FormType struct {
	Path        string
	Title       string
	Description string
	ButtonText  string
	StartURL    string
}

// Update the conversation state to track language
type ConversationState struct {
	Messages []openai.ChatCompletionMessage
	Language string
}

var conversations = make(map[string]*ConversationState)

// Add these form template structures
type RegistrationFormData struct {
	FullName      string
	Address       string
	InsuranceName string
	GroupNumber   string
	DateCompleted string
}

type VisitFormData struct {
	FullName       string
	ReasonForVisit string
	DateCompleted  string
}

// Add form templates
const registrationFormTemplate = `
PATIENT REGISTRATION FORM
------------------------
Full Name: {{.FullName}}
Address: {{.Address}}
Insurance Provider: {{.InsuranceName}}
Insurance Group #: {{.GroupNumber}}
Date: {{.DateCompleted}}
`

const visitFormTemplate = `
PATIENT VISIT FORM
-----------------
Patient Name: {{.FullName}}
Reason for Visit: {{.ReasonForVisit}}
Date: {{.DateCompleted}}
`

func generateQRCodePNG(url string) ([]byte, error) {
	qr, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	return qr.PNG(256)
}

func isFormComplete(response string) bool {
	lowered := strings.ToLower(response)
	return strings.Contains(lowered, "form is complete") ||
		strings.Contains(lowered, "form complete") ||
		strings.Contains(lowered, "formulario completo") ||
		strings.Contains(lowered, "formulaire complet") ||
		strings.Contains(lowered, "表格完成")
}

func saveForm(formType string, messages []openai.ChatCompletionMessage) error {
	// Extract form data using OpenAI
	client := openai.NewClient(os.Getenv("OPENAI_API_KEY"))

	var extractPrompt string
	if formType == "register" {
		extractPrompt = `Extract these fields from the conversation in JSON format:
{
    "FullName": "",
    "Address": "",
    "InsuranceName": "",
    "GroupNumber": ""
}`
	} else {
		extractPrompt = `Extract these fields from the conversation in JSON format:
{
    "FullName": "",
    "ReasonForVisit": ""
}`
	}

	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: openai.GPT3Dot5Turbo,
			Messages: append(messages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: extractPrompt,
			}),
		},
	)
	if err != nil {
		return fmt.Errorf("error extracting form data: %v", err)
	}

	// Parse the extracted data and create form
	var tmpl *template.Template
	var formData interface{}

	if formType == "register" {
		var regData RegistrationFormData
		if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &regData); err != nil {
			return fmt.Errorf("error parsing registration data: %v", err)
		}
		regData.DateCompleted = time.Now().Format("January 2, 2006")
		formData = regData
		tmpl = template.Must(template.New("form").Parse(registrationFormTemplate))
	} else {
		var visitData VisitFormData
		if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &visitData); err != nil {
			return fmt.Errorf("error parsing visit data: %v", err)
		}
		visitData.DateCompleted = time.Now().Format("January 2, 2006")
		formData = visitData
		tmpl = template.Must(template.New("form").Parse(visitFormTemplate))
	}

	// Create the filled form
	var formBuffer bytes.Buffer
	if err := tmpl.Execute(&formBuffer, formData); err != nil {
		return fmt.Errorf("error executing template: %v", err)
	}

	// Save to file
	timestamp := time.Now().Format("20060102150405")
	filename := filepath.Join("forms", fmt.Sprintf("%s_%s.txt", formType, timestamp))

	if err := os.WriteFile(filename, formBuffer.Bytes(), 0644); err != nil {
		return fmt.Errorf("error saving form: %v", err)
	}

	return nil
}

func main() {
	// Check for API key
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY environment variable is required")
	}

	// Initialize OpenAI client
	client := openai.NewClient(apiKey)

	// Create forms directory
	if err := os.MkdirAll("forms", 0755); err != nil {
		log.Fatal("Could not create forms directory:", err)
	}

	// Define available forms
	forms := []FormType{
		{
			Path:        "/register",
			Title:       "New Patient Registration",
			Description: "Complete this form to register as a new patient. We'll need your personal and insurance information.",
			ButtonText:  "Registration",
			StartURL:    "/startForm/register",
		},
		{
			Path:        "/visit",
			Title:       "Visit Form",
			Description: "Complete this form for today's visit. Just tell us your name and reason for visiting.",
			ButtonText:  "Visit",
			StartURL:    "/startForm/visit",
		},
	}

	// Add QR code endpoint
	http.HandleFunc("/qr/", func(w http.ResponseWriter, r *http.Request) {
		formType := strings.TrimPrefix(r.URL.Path, "/qr/")
		baseURL := "http://localhost:8080/"
		fullURL := baseURL + formType

		png, err := generateQRCodePNG(fullURL)
		if err != nil {
			http.Error(w, "Failed to generate QR code", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400") // Cache for 24 hours
		w.Write(png)
	})

	// Serve home page
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		tmpl := template.Must(template.New("home").Parse(homeTemplate))
		tmpl.Execute(w, forms)
	})

	// Add startForm handler with name parameter support
	http.HandleFunc("/startForm/", func(w http.ResponseWriter, r *http.Request) {
		formType := strings.TrimPrefix(r.URL.Path, "/startForm/")
		name := r.URL.Query().Get("name") // Get name from query parameter

		data := map[string]string{
			"formType": formType,
			"name":     name,
		}

		tmpl := template.Must(template.New("chat").Parse(chatTemplate))
		tmpl.Execute(w, data)
	})

	// Handle form pages
	http.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		tmpl := template.Must(template.New("chat").Parse(chatTemplate))
		tmpl.Execute(w, map[string]string{"formType": "register"})
	})

	http.HandleFunc("/visit", func(w http.ResponseWriter, r *http.Request) {
		tmpl := template.Must(template.New("chat").Parse(chatTemplate))
		tmpl.Execute(w, map[string]string{"formType": "visit"})
	})

	// Handle chat endpoints
	http.HandleFunc("/register/chat", func(w http.ResponseWriter, r *http.Request) {
		handleChat(w, r, client, "register")
	})

	http.HandleFunc("/visit/chat", func(w http.ResponseWriter, r *http.Request) {
		handleChat(w, r, client, "visit")
	})

	log.Println("Server starting on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleChat(w http.ResponseWriter, r *http.Request, client *openai.Client, formType string) {
	var userMessage struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&userMessage); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Get or initialize conversation state
	conv, exists := conversations[formType]
	if !exists {
		conv = &ConversationState{
			Messages: []openai.ChatCompletionMessage{},
			Language: "en", // default to English initially
		}
		conversations[formType] = conv
	}

	// Detect language from user message
	detectedLang, err := detectLanguage(client, userMessage.Message)
	if err != nil {
		log.Printf("Error detecting language: %v", err)
	} else if detectedLang != conv.Language {
		// Language has changed, update conversation state
		conv.Language = detectedLang

		// Get translated form template
		translatedTemplate, err := translateFormTemplate(client, getFormTemplate(formType), detectedLang)
		if err != nil {
			log.Printf("Error translating template: %v", err)
			translatedTemplate = getFormTemplate(formType)
		}

		// Update system message with new language and translated template
		systemMsg := fmt.Sprintf(`You are helping patients fill out this form:
%s

Important:
1. Communicate ONLY in %s
2. Ask one question at a time
3. Keep form data in English internally
4. After completion:
   - Say "Form is complete" in %s
   - Show the completed form using the template
   - Provide a summary in %s`,
			translatedTemplate, detectedLang, detectedLang, detectedLang)

		// Update system message in conversation
		if len(conv.Messages) > 0 && conv.Messages[0].Role == openai.ChatMessageRoleSystem {
			conv.Messages[0].Content = systemMsg
		} else {
			conv.Messages = append([]openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: systemMsg,
				},
			}, conv.Messages...)
		}
	}

	// Add user message
	conv.Messages = append(conv.Messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: userMessage.Message,
	})

	// Get OpenAI response
	resp, err := client.CreateChatCompletion(
		context.Background(),

		openai.ChatCompletionRequest{
			Model:    openai.GPT3Dot5Turbo,
			Messages: conv.Messages,
		},
	)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	botResponse := resp.Choices[0].Message.Content
	conv.Messages = append(conv.Messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: botResponse,
	})

	// Check if form is complete
	if isFormComplete(botResponse) {
		if err := saveForm(formType, conv.Messages); err != nil {
			log.Printf("Error saving form: %v", err)
		}

		// Extract name from messages if this is registration
		if formType == "register" {
			name := extractName(conv.Messages)
			visitURL := fmt.Sprintf("http://localhost:8080/startForm/visit?name=%s",
				url.QueryEscape(name))
			botResponse += "\n\nYou can now fill out a visit form: " + visitURL
		}

		delete(conversations, formType)
	}

	// Send response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"response": botResponse,
	})
}

func getSystemPrompt(formType string) string {
	switch formType {
	case "register":
		return fmt.Sprintf(`You are helping patients fill out this registration form:
%s

Ask one question at a time to collect all fields.
Adapt to the patient's language if they respond in a different language, but keep the form data in English.
After all fields are complete:
1. Say exactly "Form is complete"
2. Show the completed form using the template above
3. Provide a summary in the patient's preferred language`, registrationFormTemplate)

	case "visit":
		return fmt.Sprintf(`You are helping patients fill out this visit form:
%s

The patient's name will be provided first. Then ask for the Reason for Visit.
Adapt to the patient's language if they respond in a different language, but keep the form data in English.
After all fields are complete:
1. Say exactly "Form is complete"
2. Show the completed form using the template above
3. Provide a summary in the patient's preferred language`, visitFormTemplate)

	default:
		return ""
	}
}

const homeTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Medical Forms</title>
    <style>
        body { font-family: Arial, sans-serif; max-width: 800px; margin: 20px auto; padding: 0 20px; }
        .form-card { 
            border: 1px solid #ccc; 
            padding: 20px; 
            margin: 20px 0; 
            border-radius: 5px;
            display: flex;
            align-items: center;
            gap: 20px;
        }
        .form-info { flex-grow: 1; }
        .qr-code { 
            width: 150px; 
            height: 150px;
            object-fit: contain;
            cursor: pointer;
        }
        h2 { margin-top: 0; }
        a { text-decoration: none; color: inherit; }
        .button {
            display: inline-block;
            padding: 10px 20px;
            background-color: #4CAF50;
            color: white;
            border-radius: 5px;
            margin-top: 10px;
        }
    </style>
</head>
<body>
    <h1>Available Forms</h1>
    {{range .}}
    <div class="form-card">
        <div class="form-info">
            <h2>{{.Title}}</h2>
            <p>{{.Description}}</p>
            <a href="{{.StartURL}}" class="button">{{.ButtonText}}</a>
        </div>
        <a href="{{.StartURL}}">
            <img src="/qr/{{.Path}}" class="qr-code" alt="QR Code for {{.Title}}">
        </a>
    </div>
    {{end}}
</body>
</html>
`

const chatTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>{{.formType}} Form</title>
    <style>
        body { font-family: Arial, sans-serif; max-width: 800px; margin: 20px auto; padding: 0 20px; }
        #chat-container { height: 500px; border: 1px solid #ccc; overflow-y: auto; padding: 20px; margin-bottom: 20px; }
        #input-container { display: flex; gap: 10px; }
        #message-input { flex-grow: 1; padding: 10px; }
        button { padding: 10px 20px; background-color: #4CAF50; color: white; border: none; cursor: pointer; }
        .message { margin-bottom: 10px; padding: 10px; border-radius: 5px; }
        .user { background-color: #e3f2fd; }
        .bot { background-color: #f5f5f5; }
        .bot a { color: #2196F3; text-decoration: underline; }
    </style>
</head>
<body>
    <h1>{{.formType}} Form</h1>
    <div id="chat-container"></div>
    <div id="input-container">
        <input type="text" id="message-input" placeholder="Type your response...">
        <button onclick="sendMessage()">Send</button>
    </div>

    <script>
        const chatContainer = document.getElementById('chat-container');
        const messageInput = document.getElementById('message-input');
        const formType = "{{.formType}}";
        const preFillName = "{{.name}}";

        // Start the conversation with name awareness
        function startConversation() {
            if (formType === "visit" && preFillName) {
                // If we have the name for visit form, use it and ask for reason
                addMessage("Welcome back, " + preFillName + "! What brings you in today?", 'bot');
                
                // Automatically send the name to the server
                fetch('/' + formType + '/chat', {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                    },
                    body: JSON.stringify({ message: preFillName })
                })
                .then(response => response.json())
                .then(data => {
                    // The bot should now ask about reason for visit
                    addMessage(data.response, 'bot');
                })
                .catch(error => {
                    addMessage('Error: Could not process name', 'bot');
                });
            } else {
                // Otherwise ask for name as usual
                addMessage("Hello! What is your name?", 'bot');
            }
            messageInput.focus();
        }

        startConversation();

        messageInput.addEventListener('keypress', function(e) {
            if (e.key === 'Enter') sendMessage();
        });

        async function sendMessage() {
            const message = messageInput.value.trim();
            if (!message) return;

            addMessage(message, 'user');
            messageInput.value = '';

            try {
                const response = await fetch('/' + formType + '/chat', {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                    },
                    body: JSON.stringify({ message: message })
                });
                
                const data = await response.json();
                addMessage(data.response, 'bot');
            } catch (error) {
                addMessage('Error: Could not get response', 'bot');
            }
        }

        function addMessage(text, sender) {
            const div = document.createElement('div');
            div.className = 'message ' + sender;
            
            // Convert URLs to clickable links
            if (sender === 'bot') {
                const urlRegex = /(https?:\/\/[^\s]+)/g;
                div.innerHTML = text.replace(urlRegex, '<a href="$1" target="_blank">$1</a>');
            } else {
                div.textContent = text;
            }
            
            chatContainer.appendChild(div);
            chatContainer.scrollTop = chatContainer.scrollHeight;
        }
    </script>
</body>
</html>
`

// Add a function to extract name from messages
func extractName(messages []openai.ChatCompletionMessage) string {
	// Simple implementation - you might want to make this more sophisticated
	for _, msg := range messages {
		if msg.Role == openai.ChatMessageRoleUser {
			// Assume the first user message is their name
			return msg.Content
		}
	}
	return ""
}

func detectLanguage(client *openai.Client, text string) (string, error) {
	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: openai.GPT3Dot5Turbo,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: "Detect the language of the following text. Respond with only the ISO language code (e.g., 'es' for Spanish, 'fr' for French).",
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: text,
				},
			},
		},
	)
	if err != nil {
		return "en", err
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

func translateFormTemplate(client *openai.Client, template string, langCode string) (string, error) {
	if langCode == "en" {
		return template, nil
	}

	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: openai.GPT3Dot5Turbo,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: fmt.Sprintf("Translate the following HTML form template to %s. Keep HTML tags and {{.Variables}} unchanged.", langCode),
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: template,
				},
			},
		},
	)
	if err != nil {
		return template, err
	}
	return resp.Choices[0].Message.Content, nil
}

func getFormTemplate(formType string) string {
	switch formType {
	case "register":
		return `
<div class="completed-form">
    <h2>PATIENT REGISTRATION FORM</h2>
    <table class="form-table">
        <tr>
            <td class="label">Full Name:</td>
            <td class="value">{{.FullName}}</td>
        </tr>
        <tr>
            <td class="label">Address:</td>
            <td class="value">{{.Address}}</td>
        </tr>
        <tr>
            <td class="label">Insurance Provider:</td>
            <td class="value">{{.InsuranceName}}</td>
        </tr>
        <tr>
            <td class="label">Insurance Group #:</td>
            <td class="value">{{.GroupNumber}}</td>
        </tr>
        <tr>
            <td class="label">Date:</td>
            <td class="value">{{.DateCompleted}}</td>
        </tr>
    </table>
</div>`
	case "visit":
		return `
<div class="completed-form">
    <h2>PATIENT VISIT FORM</h2>
    <table class="form-table">
        <tr>
            <td class="label">Patient Name:</td>
            <td class="value">{{.FullName}}</td>
        </tr>
        <tr>
            <td class="label">Reason for Visit:</td>
            <td class="value">{{.ReasonForVisit}}</td>
        </tr>
        <tr>
            <td class="label">Date:</td>
            <td class="value">{{.DateCompleted}}</td>
        </tr>
    </table>
</div>`
	default:
		return ""
	}
}
