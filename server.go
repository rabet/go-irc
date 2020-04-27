package main

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/rsms/gotalk"
	redisStore "gopkg.in/boj/redistore.v1"
)

type Room struct {
	Name     string `json:"name"`
	mu       sync.RWMutex
	messages []*Message
}

func (room *Room) appendMessage(m *Message) {
	room.mu.Lock()
	defer room.mu.Unlock()
	room.messages = append(room.messages, m)
}

type Message struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

type NewMessage struct {
	Room    string  `json:"room"`
	Message Message `json:"message"`
}

type RoomMap map[string]*Room

const (
	SessionName = "go-irc"
	ValueName   = "goirc-username"
)

var (
	rooms        RoomMap
	roomsmu      sync.RWMutex
	socks        map[*gotalk.Sock]int
	socksmu      sync.RWMutex
	sessionStore *redisStore.RediStore
	user         interface{}
)

func onAccept(s *gotalk.Sock) {
	// Keep track of connected sockets
	socksmu.Lock()
	defer socksmu.Unlock()
	socks[s] = 1

	s.CloseHandler = func(s *gotalk.Sock, _ int) {
		socksmu.Lock()
		defer socksmu.Unlock()
		delete(socks, s)
	}

	// Send list of rooms
	roomsmu.RLock()
	defer roomsmu.RUnlock()
	s.Notify("rooms", rooms)

	// Assign the socket a random username
	username := user // randomName()
	s.UserData = username
	s.Notify("username", username)
}

func broadcast(name string, in interface{}) {
	socksmu.RLock()
	defer socksmu.RUnlock()
	for s, _ := range socks {
		s.Notify(name, in)
	}
}

func findRoom(name string) *Room {
	roomsmu.RLock()
	defer roomsmu.RUnlock()
	return rooms[name]
}

func createRoom(name string) *Room {
	roomsmu.Lock()
	defer roomsmu.Unlock()
	room := rooms[name]
	if room == nil {
		room = &Room{Name: name}
		rooms[name] = room
		broadcast("rooms", rooms)
	}
	return room
}

// Instead of asking the user for her/his name, we randomly assign one
var names []string

func randomName() string {
	first := names[rand.Intn(len(names))]
	return first
	// last := names.Last[rand.Intn(len(names.Last))][:1]
	// return first + " " + last
}

/* SESSIONS */
func handleSessionError(w http.ResponseWriter, err error) {
	// log.WithField("err", err).Info("Error handling session.")
	http.Error(w, "Application Error", http.StatusInternalServerError)
}

func home(w http.ResponseWriter, r *http.Request) {
	session, err := sessionStore.Get(r, SessionName)
	if err != nil {
		handleSessionError(w, err)
		return
	}

	username, found := session.Values[ValueName]
	if !found || username == "" {
		http.Redirect(w, r, "/www/login.html", http.StatusSeeOther)
		// log.WithField("username", username).Info("Username is empty/notfound, redirecting")
		return
	}

	user = username
	w.Header().Add("Content-Type", "text/html")
	http.Redirect(w, r, "/www/index.html", 303)
	// fmt.Fprintf(w, "<html><body>Hello %s<br/><a href='/logout'>Logout</a></body></html>", username)
}

func login(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	if username == "" {
		username = randomName()
	}
	// password := r.FormValue("password")

	// log.WithFields(logrus.Fields{"username": username, "password": password}).Info("Received login request.")

	// Normally, these would probably be looked up in a DB or environment
	if username != "" {
		session, err := sessionStore.Get(r, SessionName)
		if err != nil {
			handleSessionError(w, err)
			return
		}

		session.Values[ValueName] = username
		if err := session.Save(r, w); err != nil {
			handleSessionError(w, err)
			return
		}

		// log.WithField("username", username).Info("completed login & session.Save")
	}

	http.Redirect(w, r, "/", 303)
}

func logout(w http.ResponseWriter, r *http.Request) {
	session, err := sessionStore.Get(r, SessionName)
	if err != nil {
		handleSessionError(w, err)
		return
	}

	session.Values[ValueName] = ""
	if err := session.Save(r, w); err != nil {
		handleSessionError(w, err)
		return
	}

	// log.Info("completed logout & session.Save")
	http.Redirect(w, r, "/", 302)
}

func determineEncryptionKey() ([]byte, error) {
	sek := os.Getenv("SESSION_ENCRYPTION_KEY")
	lek := len(sek)
	switch {
	case lek >= 0 && lek < 16, lek > 16 && lek < 24, lek > 24 && lek < 32:
		return nil, errors.New("SESSION_ENCRYPTION_KEY needs to be either 16, 24 or 32 characters long or longer")
	case lek == 16, lek == 24, lek == 32:
		return []byte(sek), nil
	case lek > 32:
		return []byte(sek[0:32]), nil
	default:
		return nil, errors.New("invalid SESSION_ENCRYPTION_KEY: " + sek)
	}
}

func main() {
	socks = make(map[*gotalk.Sock]int)
	rooms = make(RoomMap)

	ek, _ := determineEncryptionKey()
	sessionStore, _ = redisStore.NewRediStore(
		5,
		"tcp",
		os.Getenv("REDIS_HOST"),
		os.Getenv("REDIS_PWD"),
		[]byte(os.Getenv("SESSION_AUTHENTICATION_KEY")), // os.Getenv("SESSION_AUTHENTICATION_KEY")
		ek,
	)

	// Load names data
	if namesjson, err := ioutil.ReadFile("names.json"); err != nil {
		panic("failed to read names.json: " + err.Error())
	} else if err := json.Unmarshal(namesjson, &names); err != nil {
		panic("failed to read names.json: " + err.Error())
	}
	rand.Seed(time.Now().UTC().UnixNano())

	// Add some sample rooms and messages
	createRoom("random").appendMessage(
		&Message{randomName(), "I like cats"})
	createRoom("inspirace").appendMessage(
		&Message{randomName(), "Two tomatoes walked across the street ..."})
	createRoom("pineapple :)").appendMessage(
		&Message{randomName(), "func(func(func(func())func()))func()"})

	// Register our handlers
	gotalk.Handle("list-messages", func(roomName string) ([]*Message, error) {
		room := findRoom(roomName)
		if room == nil {
			return nil, errors.New("no such room")
		}
		return room.messages, nil
	})

	gotalk.Handle("send-message", func(s *gotalk.Sock, r NewMessage) error {
		if len(r.Message.Body) == 0 {
			return errors.New("empty message")
		}
		username, _ := s.UserData.(string)
		room := findRoom(r.Room)
		room.appendMessage(&Message{username, r.Message.Body})
		r.Message.Author = username
		broadcast("newmsg", &r)
		return nil
	})

	gotalk.Handle("create-room", func(name string) (*Room, error) {
		if len(name) == 0 {
			return nil, errors.New("empty name")
		}
		return createRoom(name), nil
	})

	// Serve gotalk at "/gotalk/"
	gotalkws := gotalk.WebSocketHandler()
	gotalkws.OnAccept = onAccept
	http.Handle("/gotalk/", gotalkws)

	http.Handle("/www/", http.StripPrefix("/www/", http.FileServer(http.Dir("./www"))))
	http.HandleFunc("/login", login)
	http.HandleFunc("/logout", logout)
	http.HandleFunc("/", home)
	port := os.Getenv("PORT")
	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		panic("ListenAndServe: " + err.Error())
	}
}
