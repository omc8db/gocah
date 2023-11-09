package main

import (
	"bufio"
	"bytes"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/r3labs/sse/v2"
)

const (
	InitialHand = 7
)

var (
	games = make(map[string]*Game)
	//go:embed templates/*
	templateFS embed.FS
	templates  = template.Must(template.ParseFS(templateFS, "templates/*.html"))
	rng        = rand.New(rand.NewSource(time.Now().UnixNano()))
	blackDeck  []string
	whiteDeck  []string
	updates    *sse.Server
)

func main() {
	blackfile := flag.String("black", "blackcards.txt", "Path to file with black deck")
	whitefile := flag.String("white", "whitecards.txt", "Path to file with white deck")
	bind := flag.String("bind", ":8080", "TCP addr:port to start the server on")
	flag.Parse()
	blackDeck, whiteDeck = readDeck(*blackfile, *whitefile)
	fmt.Printf("Loaded deck from %s, %s\n", *blackfile, *whitefile)
	url := "http://" + *bind + "/"
	if (*bind)[0] == ':' {
		url = "http://localhost" + *bind + "/"
	}
	fmt.Println("Serving on ", url)

	updates = sse.New()
	http.HandleFunc("/", landingPage)
	http.HandleFunc("/updates", updates.ServeHTTP)
	http.HandleFunc("/game", handleGameRequest)
	http.HandleFunc("/submit", handleSubmit)
	http.HandleFunc("/choose", handleChooseWinner)
	log.Fatal(http.ListenAndServe(*bind, nil))
}

func readDeck(blackFile string, whiteFile string) (black []string, white []string) {
	var results [][]string
	infiles := []string{blackFile, whiteFile}
	for _, fname := range infiles {
		file, err := os.Open(fname)
		panicIfErr(err)
		defer file.Close()

		sc := bufio.NewScanner(file)
		lines := make([]string, 0)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		panicIfErr(sc.Err())
		results = append(results, lines)
	}
	return results[0], results[1]
}

// ---- Communication Hooks ----
func landingPage(w http.ResponseWriter, r *http.Request) {
	logIfErr(templates.ExecuteTemplate(w, "landing.html", ""))
}

func handleGameRequest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	gameName := q.Get("name")
	playerName := q.Get("player")
	if gameName == "" || playerName == "" {
		http.Error(w, "Specify game and player name", http.StatusBadRequest)
		return
	}
	fmt.Printf("player %s connected to game %s\n", playerName, gameName)
	view := enterGame(gameName, playerName)
	logIfErr(templates.ExecuteTemplate(w, "game.html", view))
}

func handleSubmit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	player := q.Get("player")
	game := games[q.Get("game")]
	card, err := strconv.Atoi(q.Get("card"))
	if player == "" || game == nil || err != nil {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}
	err = game.submitCard(player, card)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func handleChooseWinner(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	player := q.Get("player")
	game := games[q.Get("game")]
	if player == "" || game == nil {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}
	game.chooseWinner(player)
}

// Server Sent Event updates
// Re-render the scoreboard + black card and send it to all players
func (g *Game) updateHeader() {
	var buf bytes.Buffer
	logIfErr(templates.ExecuteTemplate(&buf, "header.html", *g))
	// Bug in the SSE library? Looks like it can't handle messages
	// with newlines, which is bad if the contents are HTML.
	stripped := bytes.Replace(buf.Bytes(), []byte("\n"), []byte(" "), -1)
	for _, p := range g.Players {
		updates.Publish(g.Name+"_"+p.Name, &sse.Event{
			Data:  stripped,
			Event: []byte("header"),
		})
	}
}

func (g *Game) updateHands() {
	for _, p := range g.Players {
		sseTopic := g.Name + "_" + p.Name
		sseEvent := sse.Event{Event: []byte("hand")}
		if p.Name == g.CardCzar {
			sseEvent.Data = []byte("<li style=\"background-color: bisque\">You are the card czar.<br><br>Waiting for all players to submit a card</li>")
		} else {
			var buf bytes.Buffer
			logIfErr(templates.ExecuteTemplate(&buf, "hand.html", GameView{p, *g}))
			sseEvent.Data = bytes.Replace(buf.Bytes(), []byte("\n"), []byte(" "), -1)
		}
		updates.Publish(sseTopic, &sseEvent)
	}
}

// ---- Game data structures ----
type Player struct {
	Name  string
	Score int
	Cards []string
}
type submission struct {
	Player string
	Card   string
}
type Game struct {
	Name      string
	Players   []Player
	Question  string
	CardCzar  string
	Submitted []submission

	// Card indexes in black/white deck.
	// Starts out shuffled, each draw pops a card off the end of the slice
	Blackdeck []string
	Whitedeck []string
}

// Used by templates to render the game from a specific player's view
// Templates don't suppor dereferencing pointers
// so this has to be a whole copy
type GameView struct {
	Player Player
	Game   Game
}

// ---- Game Logic ----

// Connect a player to a game. Creates the game and player if they don't exist
func enterGame(gameName string, playerName string) GameView {
	game, ok := games[gameName]
	if !ok {
		game = newGame(gameName)
		fmt.Println("Created a new game: ", gameName)
		games[gameName] = game
	}
	player := game.upsertPlayer(playerName)
	return GameView{player, *game}
}

func newGame(name string) *Game {
	game := Game{Name: name}
	game.Submitted = make([]submission, 0, 10)
	game.Players = make([]Player, 0, 10)

	// Shuffle the deck
	game.Blackdeck = make([]string, len(blackDeck))
	for i, j := range rng.Perm(len(blackDeck)) {
		game.Blackdeck[i] = blackDeck[j]
	}
	game.Whitedeck = make([]string, len(whiteDeck))
	for i, j := range rng.Perm(len(whiteDeck)) {
		game.Whitedeck[i] = whiteDeck[j]
	}

	// Draw the first card
	game.Question, game.Blackdeck = game.Blackdeck[0], game.Blackdeck[1:]
	return &game
}

func (g *Game) upsertPlayer(name string) Player {
	for _, player := range g.Players {
		if player.Name == name {
			return player
		}
	}
	fmt.Printf("Player %s is new to the game\n", name)
	player := Player{Name: name, Score: 0, Cards: make([]string, 0, 10)}
	// Draw initial hand
	player.Cards, g.Whitedeck = g.Whitedeck[:InitialHand], g.Whitedeck[InitialHand:]
	g.Players = append(g.Players, player)
	if len(g.Players) == 1 {
		fmt.Printf("%s is the card czar now because they are the only player\n", name)
		g.CardCzar = name
	}
	// When the second player joins, start the round
	if len(g.Players) == 2 {
		fmt.Println("Second player has joined, starting the round")
		g.updateHands()
	}
	updates.CreateStream(g.Name + "_" + name)
	g.updateHeader()
	return player
}

func (g *Game) newRound() {
	// Clear submissions
	g.Submitted = g.Submitted[:0]
	// New Question
	if len(g.Blackdeck) == 0 {
		fmt.Printf("%s: Game Over!\n", g.Name)
		return
	}
	g.Question, g.Blackdeck = g.Blackdeck[0], g.Blackdeck[1:]
	// New Card Czar
	// I tried doing this as an index but go templating can't do index lookups to
	// render state.
	for i, p := range g.Players {
		if p.Name == g.CardCzar {
			g.CardCzar = g.Players[(i+1)%len(g.Players)].Name
			break
		}
	}
	g.updateHeader()
	// If possible new hand of white cards
	if len(g.Whitedeck) < len(g.Players) {
		fmt.Printf("Can't deal hand in game %s, out of cards\n", g.Name)
		return
	}
	for i, p := range g.Players {
		// A player can join while scoring is in progress, don't give them an extra card
		if len(p.Cards) >= InitialHand {
			continue
		}
		g.Players[i].Cards = append(g.Players[i].Cards, g.Whitedeck[0])
		g.Whitedeck = g.Whitedeck[1:]
	}
	g.updateHands()
}

func (g *Game) submitCard(playerName string, card int) error {
	fmt.Printf("Received submission: game %s player %s card %d\n", g.Name, playerName, card)
	// Validation
	if len(g.Players) < 2 {
		return errors.New("the round has not started yet")
	}
	var player *Player
	for i, p := range g.Players {
		if p.Name == playerName {
			player = &g.Players[i]
		}
	}
	if player == nil {
		return fmt.Errorf("player %s not in game %s", playerName, g.Name)
	}
	for _, s := range g.Submitted {
		if s.Player == playerName {
			return fmt.Errorf("player %s has already submitted a card this round", playerName)
		}
	}
	if card >= len(player.Cards) {
		return fmt.Errorf("%d Refers to nonexistent card", card)
	}
	// Valid submission, add it in and remove from hand
	g.Submitted = append(g.Submitted, submission{playerName, player.Cards[card]})
	player.Cards = append(player.Cards[:card], player.Cards[card+1:]...)
	fmt.Printf("Submission validated. There are now %d submissions, need %d\n", len(g.Submitted), len(g.Players)-1)
	// If all players are in, show the hand to the card czar
	if len(g.Submitted) == len(g.Players)-1 {
		fmt.Println(g.Name, ": all players are in, revealing to card czar")
		g.revealRound()
	}
	return nil
}

func (g *Game) revealRound() {
	var buf bytes.Buffer
	logIfErr(templates.ExecuteTemplate(&buf, "czarhand.html", *g))

	// Bug in the SSE library? Looks like it can't handle messages
	// with newlines, which is bad if the contents are HTML.
	stripped := bytes.Replace(buf.Bytes(), []byte("\n"), []byte(" "), -1)
	updates.Publish(g.Name+"_"+g.CardCzar, &sse.Event{
		Data:  stripped,
		Event: []byte("hand"),
	})
}

func (g *Game) chooseWinner(player string) {
	for i, p := range g.Players {
		if p.Name == player {
			g.Players[i].Score += 1
		}
	}
	g.newRound()
}

// ------ Misc ----
func panicIfErr(e error) {
	if e != nil {
		panic(e)
	}
}
func logIfErr(e error) {
	if e != nil {
		fmt.Println("Error! ", e.Error())
	}
}
