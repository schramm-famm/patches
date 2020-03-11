package websockets

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"patches/models"

	gorillaws "github.com/gorilla/websocket"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// Conversation manages all WebSocket connections in a single conversation.
type Conversation struct {
	conversationID int64
	doc            string
	clients        map[*Client]bool
	version        int

	register   chan *Client
	unregister chan *Client
	broadcast  chan *Message
}

// Message stores the content and sender of a message.
type Message struct {
	content []byte
	sender  *Client
}

var dmp *diffmatchpatch.DiffMatchPatch = diffmatchpatch.New()

// NewConversation creates a new Conversation struct.
func NewConversation(conversationID int64, doc string) *Conversation {
	return &Conversation{
		conversationID: conversationID,
		doc:            doc,
		clients:        make(map[*Client]bool),
		version:        0,
		register:       make(chan *Client),
		unregister:     make(chan *Client),
		broadcast:      make(chan *Message),
	}
}

func (c *Conversation) processMessage(msg models.Message) (*models.Message, error) {
	if msg.Type != models.TypeUpdate {
		errMsg := fmt.Sprintf("Message is not of type %d", models.TypeUpdate)
		return nil, errors.New(errMsg)
	}

	update := msg.Data
	if update.Type == nil {
		errMsg := fmt.Sprintf(`Update is missing required "type" field in "data"`)
		return nil, errors.New(errMsg)
	}

	switch *update.Type {
	case models.UpdateTypeEdit:
		if update.Type == nil || update.Version == nil || update.Patch == nil || update.CursorDelta == nil {
			errMsg := fmt.Sprintf(`Update (EDIT) is missing required fields in "data"`)
			return nil, errors.New(errMsg)
		}

		if *update.Version < 1 {
			errMsg := fmt.Sprintf("Update has invalid version number %d", update.Version)
			return nil, errors.New(errMsg)
		}

		patches, err := dmp.PatchFromText(*update.Patch)
		if err != nil {
			return nil, err
		}
		if len(patches) != 1 {
			errMsg := "Update must contain one patch"
			return nil, errors.New(errMsg)
		}

		newDoc, okList := dmp.PatchApply(patches, c.doc)
		if !okList[0] {
			return nil, nil
		}
		c.doc = newDoc

		if *update.Version != c.version+1 {
			*update.Version = c.version + 1
		}

	case models.UpdateTypeCursor:
		if update.Type == nil || update.CursorDelta == nil {
			errMsg := fmt.Sprintf(`Update (CURSOR) is missing required fields in "data"`)
			return nil, errors.New(errMsg)
		}

	default:
		errMsg := fmt.Sprintf("Update has invalid sub-type %d", update.Type)
		return nil, errors.New(errMsg)
	}

	return &msg, nil
}

func (c *Conversation) registerClient(client *Client) {
	// Create and send Init message to the new client
	init := models.Message{
		Type: models.TypeInit,
		Data: models.InnerData{
			Version: &c.version,
			Content: &c.doc,
		},
	}
	if len(c.clients) > 0 {
		activeUsers := make(map[int64]int)
		for client := range c.clients {
			activeUsers[client.userID] = client.position
		}
		init.Data.ActiveUsers = &activeUsers
	}
	initMessage, err := json.Marshal(init)
	if err != nil {
		log.Print("Failed to encode initial conversation data: ", err)
		close(client.send)
		return
	}
	err = client.conn.WriteMessage(gorillaws.TextMessage, initMessage)
	if err != nil {
		log.Print("Failed to send initial conversation data: ", err)
		close(client.send)
		return
	}

	// Create and broadcast UserJoin message to all existing clients
	userJoinMsg := models.Message{
		Type: models.TypeUserJoin,
		Data: models.InnerData{
			UserID: &client.userID,
		},
	}
	broadcastMessageBytes, err := json.Marshal(userJoinMsg)
	if err != nil {
		log.Printf("Failed to encode user joining message as byte array: %v", err)
		close(client.send)
		return
	}
	for client := range c.clients {
		client.send <- broadcastMessageBytes
	}

	c.clients[client] = true
	log.Printf("Registered a client in conversation %d (%d active)", c.conversationID, len(c.clients))
}

func (c *Conversation) unregisterClient(client *Client) {
	if _, ok := c.clients[client]; !ok {
		log.Printf("Attempted to unregister an inactive client in conversation %d", c.conversationID)
		return
	}

	delete(c.clients, client)
	close(client.send)
	log.Printf("Unregistered a client in conversation %d (%d active)", c.conversationID, len(c.clients))

	// Create and broadcast UserLeave message to all existing clients
	userLeaveMsg := models.Message{
		Type: models.TypeUserLeave,
		Data: models.InnerData{
			UserID: &client.userID,
		},
	}
	broadcastMessageBytes, err := json.Marshal(userLeaveMsg)
	if err != nil {
		log.Printf("Failed to encode user leaving message as byte array: %v", err)
		return
	}
	for client := range c.clients {
		client.send <- broadcastMessageBytes
	}
}

func (c *Conversation) processBroadcast(message *Message) {
	if _, ok := c.clients[message.sender]; !ok {
		log.Printf("Attempted to broadcast from an inactive client in conversation %d", c.conversationID)
		return
	}

	msg := models.Message{}
	if err := json.Unmarshal(message.content, &msg); err != nil {
		log.Printf("Failed to parse WebSocket message content: %v", err)
		c.unregisterClient(message.sender)
		return
	}

	newMsg, err := c.processMessage(msg)
	if err != nil {
		log.Printf("Failed to process update: %v", err)
		c.unregisterClient(message.sender)
		return
	}
	if newMsg == nil {
		log.Printf("Patch %s could not be applied", *msg.Data.Patch)
		return
	}

	msg.Data.UserID = &message.sender.userID
	broadcastMessageBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to encode update message as byte array: %v", err)
		c.unregisterClient(message.sender)
		return
	}

	for client := range c.clients {
		if client != message.sender {
			client.send <- broadcastMessageBytes
		}
	}
	message.sender.position += *msg.Data.CursorDelta

	if *msg.Data.Type == models.UpdateTypeEdit {
		c.version++

		ackMessage := models.Message{
			Type: models.TypeAck,
			Data: models.InnerData{
				Version: msg.Data.Version,
			},
		}
		ackMessageBytes, err := json.Marshal(ackMessage)
		if err != nil {
			log.Printf("Failed to encode acknowledge message as byte array: %v", err)
			c.unregisterClient(message.sender)
			return
		}
		message.sender.send <- ackMessageBytes
	}
}

// Run waits on a Conversation's three channels for clients to be added, clients
// to be removed, and messages to be broadcast. Only one of these operations may
// be performed at a time.
func (c *Conversation) Run() {
	for {
		select {
		case client := <-c.register:
			c.registerClient(client)

		case client := <-c.unregister:
			c.unregisterClient(client)

		case message, ok := <-c.broadcast:
			if !ok {
				close(c.register)
				close(c.unregister)
				log.Printf("Shutting down conversation %d", c.conversationID)
				return
			}
			c.processBroadcast(message)

		}
	}
}
