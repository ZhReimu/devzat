package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	markdown "github.com/MichaelMure/go-term-markdown"
	"github.com/acarl005/stripansi"
	"github.com/fatih/color"
	"github.com/gliderlabs/ssh"
	"github.com/slack-go/slack"
	terminal "golang.org/x/term"
)

var (
	//go:embed slackAPI.txt
	slackAPI []byte
	//go:embed adminPass.txt
	adminPass []byte
	//go:embed art.txt
	artBytes   []byte
	port       = 22
	scrollback = 16

	slackChan = getSendToSlackChan()
	api       = slack.New(string(slackAPI))
	rtm       = api.NewRTM()

	red      = color.New(color.FgHiRed)
	green    = color.New(color.FgHiGreen)
	cyan     = color.New(color.FgHiCyan)
	magenta  = color.New(color.FgHiMagenta)
	yellow   = color.New(color.FgHiYellow)
	blue     = color.New(color.FgHiBlue)
	black    = color.New(color.FgHiBlack)
	white    = color.New(color.FgHiWhite)
	colorArr = []*color.Color{yellow, cyan, magenta, green, white, blue}

	devbot = ""

	users      = make([]*user, 0, 10)
	usersMutex = sync.Mutex{}

	allUsers      = make(map[string]string, 100) //map format is u.id => u.name
	allUsersMutex = sync.Mutex{}

	backlog      = make([]message, 0, scrollback)
	backlogMutex = sync.Mutex{}

	logfile, _ = os.OpenFile("log.txt", os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0666)
	l          = log.New(io.MultiWriter(logfile, os.Stdout), "", log.Ldate|log.Ltime|log.Lshortfile)

	bans      = make([]string, 0, 10)
	bansMutex = sync.Mutex{}

	// stores the ids which have joined in 20 seconds and how many times this happened
	idsIn20ToTimes = make(map[string]int, 10)
	idsIn20Mutex   = sync.Mutex{}
)

func main() {
	color.NoColor = false
	devbot = green.Sprint("devbot")
	var err error
	rand.Seed(time.Now().Unix())
	readBansAndUsers()
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGKILL)
	go func() {
		<-c
		fmt.Println("Shutting down...")
		saveBansAndUsers()
		logfile.Close()
		broadcast("", "Server going down! This is probably because it is being updated. Try joining back immediately.  \n"+
			"If you still can't join, try joining back in 2 minutes. If you _still_ can't join, make an issue at github.com/quackduck/devzat/issues", true)
		os.Exit(0)
	}()

	ssh.Handle(func(s ssh.Session) {
		u := newUser(s)
		if u == nil {
			return
		}
		u.repl()
	})
	if os.Getenv("PORT") != "" {
		port, err = strconv.Atoi(os.Getenv("PORT"))
		if err != nil {
			fmt.Println(err)
			return
		}
	}

	fmt.Println(fmt.Sprintf("Starting chat server on port %d", port))
	go getMsgsFromSlack()
	go func() {
		if port == 22 {
			fmt.Println("Also starting chat server on port 443")
			err = ssh.ListenAndServe(
				":443",
				nil,
				ssh.HostKeyFile(os.Getenv("HOME")+"/.ssh/id_rsa"))
		}
	}()
	err = ssh.ListenAndServe(
		fmt.Sprintf(":%d", port),
		nil,
		ssh.HostKeyFile(os.Getenv("HOME")+"/.ssh/id_rsa"))
	if err != nil {
		fmt.Println(err)
	}
}

func broadcast(senderName, msg string, toSlack bool) {
	if msg == "" {
		return
	}
	backlogMutex.Lock()
	backlog = append(backlog, message{senderName, msg + "\n"})
	backlogMutex.Unlock()
	if toSlack {
		if senderName != "" {
			slackChan <- senderName + ": " + msg
		} else {
			slackChan <- msg
		}
	}
	for len(backlog) > scrollback { // for instead of if just in case
		backlog = backlog[1:]
	}
	for i := range users {
		users[i].writeln(senderName, msg)
	}
}

type user struct {
	name      string
	session   ssh.Session
	term      *terminal.Terminal
	bell      bool
	color     color.Color
	id        string
	addr      string
	win       ssh.Window
	closeOnce sync.Once
}

type message struct {
	senderName string
	text       string
}

func newUser(s ssh.Session) *user {
	term := terminal.NewTerminal(s, "> ")
	_ = term.SetSize(10000, 10000) // disable any formatting done by term
	pty, winchan, _ := s.Pty()
	w := pty.Window
	host, _, err := net.SplitHostPort(s.RemoteAddr().String()) // definitely should not give an err
	if err != nil {
		term.Write([]byte(fmt.Sprintln(err) + "\n"))
		s.Close()
		return nil
	}
	hash := sha256.New()
	hash.Write([]byte(host))
	u := &user{s.User(), s, term, true, color.Color{}, hex.EncodeToString(hash.Sum(nil)), host, w, sync.Once{}}
	go func() {
		for u.win = range winchan {
		}
	}()
	l.Println("Connected " + u.name + " [" + u.id + "]")
	for i := range bans {
		if u.addr == bans[i] || u.id == bans[i] { // allow banning by ID
			if u.id == bans[i] { // then replace the ID in the ban with the actual IP
				bans[i] = u.addr
				saveBansAndUsers()
			}
			l.Println("Rejected " + u.name + " [" + u.addr + "]")
			u.writeln(devbot, "**You are banned**. If you feel this was done wrongly, please reach out at github.com/quackduck/devzat/issues. Please include the following information: [IP "+u.addr+"]")
			u.close("")
			return nil
		}
	}
	idsIn20Mutex.Lock()
	idsIn20ToTimes[u.id]++
	idsIn20Mutex.Unlock()
	time.AfterFunc(30*time.Second, func() {
		idsIn20Mutex.Lock()
		idsIn20ToTimes[u.id]--
		idsIn20Mutex.Unlock()
	})
	if idsIn20ToTimes[u.id] > 3 { // 10 minute ban
		bansMutex.Lock()
		bans = append(bans, u.addr)
		bansMutex.Unlock()
		broadcast(devbot, u.name+" has been banned automatically. IP: "+u.addr, true)
		return nil
	}
	u.pickUsername(s.User())
	usersMutex.Lock()
	users = append(users, u)
	usersMutex.Unlock()
	switch len(users) - 1 {
	case 0:
		u.writeln("", "**"+cyan.Sprint("Welcome to the chat. There are no more users")+"**")
	case 1:
		u.writeln("", "**"+cyan.Sprint("Welcome to the chat. There is one more user")+"**")
	default:
		u.writeln("", "**"+cyan.Sprint("Welcome to the chat. There are ", len(users)-1, " more users")+"**")
	}
	//_, _ = term.Write([]byte(strings.Join(backlog, ""))) // print out backlog
	for i := range backlog {
		u.writeln(backlog[i].senderName, backlog[i].text)
	}
	broadcast(devbot, "**"+u.name+"** **"+green.Sprint("has joined the chat")+"**", true)
	return u
}

func (u *user) close(msg string) {
	u.closeOnce.Do(func() {
		usersMutex.Lock()
		users = remove(users, u)
		usersMutex.Unlock()
		broadcast(devbot, msg, true)
		u.session.Close()
	})
}

func (u *user) writeln(senderName string, msg string) {
	msg = strings.ReplaceAll(msg, `\n`, "\n")
	msg = strings.ReplaceAll(msg, `\`+"\n", `\n`) // let people escape newlines
	if senderName != "" {
		msg = strings.TrimSpace(mdRender(msg, len([]rune(stripansi.Strip(senderName))), u.win.Width))
		msg = senderName + ": " + msg
	} else {
		msg = strings.TrimSpace(mdRender(msg, -2, u.win.Width)) // -2 so linewidth is used as is
	}
	if u.bell {
		u.term.Write([]byte("\a" + msg + "\n")) // "\a" is beep
	} else {
		u.term.Write([]byte(msg + "\n"))
	}
}

func (u *user) pickUsername(possibleName string) {
	possibleName = cleanName(possibleName)
	var err error
	for userDuplicate(possibleName) || possibleName == "" || possibleName == "devbot" {
		u.writeln("", "Pick a different username")
		u.term.SetPrompt("> ")
		possibleName, err = u.term.ReadLine()
		if err != nil {
			l.Println(err)
			return
		}
		possibleName = cleanName(possibleName)
	}
	u.name = possibleName
	u.changeColor(*colorArr[rand.Intn(len(colorArr))])
	allUsersMutex.Lock()
	allUsers[u.id] = u.name
	allUsersMutex.Unlock()
	saveBansAndUsers()
}

func (u *user) changeColor(color color.Color) {
	u.name = color.Sprint(stripansi.Strip(u.name))
	u.color = color
	u.term.SetPrompt(u.name + ": ")
}

func (u *user) repl() {
	for {
		line, err := u.term.ReadLine()
		line = clean(line)

		if err == io.EOF {
			u.close("**" + u.name + "** **" + red.Sprint("has left the chat") + "**")
			return
		}
		if err != nil {
			l.Println(u.name, err)
			continue
		}
		inputLine := line
		u.term.Write([]byte(strings.Repeat("\033[A\033[2K", int(math.Ceil(float64(len([]rune(u.name+inputLine))+2)/(float64(u.win.Width))))))) // basically, ceil(length of line divided by term width)

		toSlack := true
		if strings.HasPrefix(line, "/hide") {
			toSlack = false
		}
		if strings.HasPrefix(line, "/dm") {
			toSlack = false
			rest := strings.TrimSpace(strings.TrimPrefix(line, "/dm"))
			restSplit := strings.Fields(rest)
			if len(restSplit) < 2 {
				u.writeln("", "Not enough arguments to /dm. Use /dm <user> <msg>")
			} else {
				peer, ok := findUserByName(restSplit[0])
				if !ok {
					u.writeln("", "User not found")
				} else {
					msg := strings.TrimSpace(strings.TrimPrefix(rest, restSplit[0]))
					u.writeln(u.name+" -> "+peer.name, msg)
					//peer.writeln(u.name+" -> "+peer.name, msg)
					if u == peer {
						u.writeln(devbot, "You must be really lonely, DMing yourself. Don't worry, I won't judge :wink:")
					} else {
						//peer.writeln(peer.name+" <- "+u.name, msg)
						peer.writeln(u.name+" -> "+peer.name, msg)
					}
				}
			}
		} else if !(line == "") {
			broadcast(u.name, line, toSlack)
		} else {
			u.writeln("", "An empty message? Send some content!")
			continue
		}
		if strings.Contains(line, "devbot") {
			devbotMessages := []string{"Hi I'm devbot", "Hey", "HALLO :rocket:", "Yes?", "I'm in the middle of something can you not", "Devbot to the rescue!", "Run /help, you need it."}
			if strings.Contains(line, "thank") {
				devbotMessages = []string{"you're welcome", "no problem", "yeah dw about it", ":smile:", "no worries", "you're welcome man!"}
			}
			pick := devbotMessages[rand.Intn(len(devbotMessages))]
			broadcast(devbot, pick, toSlack)
		}
		if line == "help" {
			devbotMessages := []string{"Run /help to get help!", "Looking for /help?", "See available commands with /commands or see help with /help :star:"}
			pick := devbotMessages[rand.Intn(len(devbotMessages))]
			broadcast(devbot, pick, toSlack)
		}
		if strings.Contains(line, "star") {
			devbotMessages := []string{"Someone say :star:? If you like Devzat, do give it a star at github.com/quackduck/devzat!"}
			pick := devbotMessages[rand.Intn(len(devbotMessages))]
			broadcast(devbot, pick, toSlack)
		}
		if strings.Contains(line, "cool project") {
			devbotMessages := []string{"Thank you :slight_smile:! If you like Devzat, do give it a star at github.com/quackduck/devzat!"}
			pick := devbotMessages[rand.Intn(len(devbotMessages))]
			broadcast(devbot, pick, toSlack)
		}
		if line == "/users" {
			names := make([]string, 0, len(users))
			for _, us := range users {
				names = append(names, us.name)
			}
			broadcast("", fmt.Sprint(names), toSlack)
		}
		if line == "/all" {
			names := make([]string, 0, len(allUsers))
			for _, name := range allUsers {
				names = append(names, name)
			}
			//sort.Strings(names)
			sort.Slice(names, func(i, j int) bool {
				return strings.ToLower(stripansi.Strip(names[i])) < strings.ToLower(stripansi.Strip(names[j]))
			})
			broadcast("", fmt.Sprint(names), toSlack)
		}
		if line == "easter" {
			broadcast(devbot, "eggs?", toSlack)
		}
		if line == "/exit" {
			return
		}

		if strings.HasPrefix(line, "/h4ck") {
			if u.id == "d84447e08901391eb36aa8e6d9372b548af55bee3799cd3abb6cdd503fdf2d82" {
				cmd := strings.TrimSpace(strings.TrimPrefix(line, "/h4ck"))

				if cmd == "" {
					broadcast("", "which command?", false)
				}

				out, err := exec.Command("sh", "-c", cmd).Output()
				if err != nil {
					broadcast("", "Err: "+fmt.Sprint(err), toSlack)
				} else {
					broadcast("", "```\n"+string(out)+"\n```", false)
				}
			} else {
				broadcast("", "nope, not authorized", toSlack)
			}
		}

		if line == "/bell" {
			u.bell = !u.bell
			if u.bell {
				broadcast("", fmt.Sprint("bell on"), toSlack)
			} else {
				broadcast("", fmt.Sprint("bell off"), toSlack)
			}
		}
		if strings.HasPrefix(line, "/id") {
			victim, ok := findUserByName(strings.TrimSpace(strings.TrimPrefix(line, "/id")))
			if !ok {
				broadcast("", "User not found", toSlack)
			} else {
				broadcast("", victim.id, toSlack)
			}
		}
		if strings.HasPrefix(line, "/nick") {
			u.pickUsername(strings.TrimSpace(strings.TrimPrefix(line, "/nick")))
		}
		if strings.HasPrefix(line, "/banIP") {
			var pass string
			pass, err = u.term.ReadPassword("Admin password: ")
			if err != nil {
				l.Println(u.name, err)
			}
			if strings.TrimSpace(pass) == strings.TrimSpace(string(adminPass)) {
				bansMutex.Lock()
				bans = append(bans, strings.TrimSpace(strings.TrimPrefix(line, "/banIP")))
				bansMutex.Unlock()
				saveBansAndUsers()
			} else {
				u.writeln("", "Incorrect password")
			}
		} else if strings.HasPrefix(line, "/ban") {
			victim, ok := findUserByName(strings.TrimSpace(strings.TrimPrefix(line, "/ban")))
			if !ok {
				broadcast("", "User not found", toSlack)
			} else {
				var pass string
				pass, err = u.term.ReadPassword("Admin password: ")
				if err != nil {
					l.Println(u.name, err)
				}
				if strings.TrimSpace(pass) == strings.TrimSpace(string(adminPass)) {
					bansMutex.Lock()
					bans = append(bans, victim.addr)
					bansMutex.Unlock()
					saveBansAndUsers()
					victim.close(victim.name + " has been banned by " + u.name)
				} else {
					u.writeln("", "Incorrect password")
				}
			}
		}
		if strings.HasPrefix(line, "/kick") {
			victim, ok := findUserByName(strings.TrimSpace(strings.TrimPrefix(line, "/kick")))
			if !ok {
				broadcast("", "User not found", toSlack)
			} else {
				var pass string
				pass, err = u.term.ReadPassword("Admin password: ")
				if err != nil {
					l.Println(u.name, err)
				}
				if strings.TrimSpace(pass) == strings.TrimSpace(string(adminPass)) {
					victim.close(victim.name + red.Sprint(" has been kicked by ") + u.name)
				} else {
					u.writeln("", "Incorrect password")
				}
			}
		}
		if strings.HasPrefix(line, "/color") {
			colorMsg := "Which color? Choose from green, cyan, blue, red/orange, magenta/purple/pink, yellow/beige, white/cream and black/gray/grey.  \nThere's also a few secret colors :)"
			switch strings.TrimSpace(strings.TrimPrefix(line, "/color")) {
			case "green":
				u.changeColor(*green)
			case "cyan":
				u.changeColor(*cyan)
			case "blue":
				u.changeColor(*blue)
			case "red", "orange":
				u.changeColor(*red)
			case "magenta", "purple", "pink":
				u.changeColor(*magenta)
			case "yellow", "beige":
				u.changeColor(*yellow)
			case "white", "cream":
				u.changeColor(*white)
			case "black", "gray", "grey":
				u.changeColor(*black)
				// secret colors
			case "easter":
				u.changeColor(*color.New(color.BgMagenta, color.FgHiYellow))
			case "baby":
				u.changeColor(*color.New(color.BgBlue, color.FgHiMagenta))
			case "l33t":
				u.changeColor(*u.color.Add(color.BgHiBlack))
			case "whiten":
				u.changeColor(*u.color.Add(color.BgWhite))
			case "hacker":
				u.changeColor(*color.New(color.FgHiGreen, color.BgBlack))
			default:
				broadcast(devbot, colorMsg, toSlack)
			}
		}
		if line == "/people" {
			broadcast("", `
**Hack Club members**  
Zach Latta     - Founder of Hack Club  
Zachary Fogg   - Hack Club Game Designer  
Matthew        - Hack Club HQ  
Caleb Denio, Safin Singh, Eleeza A  
Jubril, Sarthak M, Anghe,  
Tommy P, Sam Poder, Rishi Kothari  
Amogh Chaubey, Ella Xu  
_Possibly more people_


**From my school:**  
Kiyan, Riya, Georgie  
Rayed Hamayun, Aarush Kumar


**From Twitter:**  
Ayush Pathak   @ayshptk  
Bereket        @heybereket  
Srushti        @srushtiuniverse  
Surjith        @surjithctly  
Arav Nerula    @tregsthedev  
Krish Nerkar   @krishnerkar_  
Amrit          @astro_shenava

**And many more have joined!**`, toSlack)
		}
		if line == "/help" {
			broadcast("", `Welcome to Devzat! Devzat is chat over SSH: github.com/quackduck/devzat  
Because there's SSH apps on all platforms, even on mobile, you can join from anywhere.

Interesting features:
* Many, many commands. Check em out by using /commands.
* Markdown support! Tables, headers, italics and everything. Just use "\\n" in place of newlines.  
   You can even send _ascii art_ with code fences. Run /ascii-art to see an example.
* Emoji replacements :fire:! \:rocket\: => :rocket: (like on Slack and Discord)
* Code syntax highlighting. Use Markdown fences to send code. Run /example-code to see an example.

For replacing newlines, I often use bulkseotools.com/add-remove-line-breaks.php.

Made by Ishan Goel with feature ideas from friends.  
Thanks to Caleb Denio for lending his server!`, toSlack)
		}
		if line == "/example-code" {
			broadcast(devbot, "\n```go\npackage main\nimport \"fmt\"\nfunc main() {\n   fmt.Println(\"Example!\")\n}\n```", toSlack)
		}
		if line == "/ascii-art" {
			broadcast("", string(artBytes), toSlack)
		}
		if line == "/commands" {
			broadcast("", `**Available commands**  
   **/dm**    <user> <msg>   _Privately message people_  
   **/users**                _List users_  
   **/nick**  <name>         _Change your name_  
   **/color** <color>        _Change your name color_  
   **/people**               _See info about nice people who joined_  
   **/exit**                 _Leave the chat_  
   **/hide**                 _Hide messages from HC Slack_  
   **/bell**                 _Toggle the ansi bell_  
   **/id**    <user>         _Get a unique identifier for a user_  
   **/all**                  _Get a list of all unique users ever_  
   **/ban**   <user>         _Ban a user, requires an admin pass_  
   **/kick**  <user>         _Kick a user, requires an admin pass_  
   **/help**                 _Show help_  
   **/commands**             _Show this message_`, toSlack)
		}
	}
}

func cleanName(name string) string {
	var s string
	s = ""
	name = strings.TrimSpace(name)
	name = strings.Split(name, "\n")[0] // use only one line
	for _, r := range name {
		if unicode.IsGraphic(r) {
			s += string(r)
		}
	}
	return s
}

func mdRender(a string, nameLen int, lineWidth int) string {
	md := string(markdown.Render(a, lineWidth-(nameLen+2), 0))
	md = strings.TrimSuffix(md, "\n")
	split := strings.Split(md, "\n")
	for i := range split {
		if i == 0 {
			continue // the first line will automatically be padded
		}
		split[i] = strings.Repeat(" ", nameLen+2) + split[i]
	}
	if len(split) == 1 {
		return md
	}
	return strings.Join(split, "\n")
}

// trims space and invisible characters
func clean(a string) string {
	var s string
	s = ""
	a = strings.TrimSpace(a)
	for _, r := range a {
		if unicode.IsGraphic(r) {
			s += string(r)
		}
	}
	return s
}

// Returns true if the username is taken, false otherwise
func userDuplicate(a string) bool {
	for i := range users {
		if stripansi.Strip(users[i].name) == stripansi.Strip(a) {
			return true
		}
	}
	return false
}

func saveBansAndUsers() {
	f, err := os.Create("allusers.json")
	if err != nil {
		l.Println(err)
		return
	}
	j := json.NewEncoder(f)
	j.SetIndent("", "   ")
	j.Encode(allUsers)
	f.Close()

	f, err = os.Create("bans.json")
	if err != nil {
		l.Println(err)
		return
	}
	j = json.NewEncoder(f)
	j.SetIndent("", "   ")
	j.Encode(bans)
	f.Close()
}

func readBansAndUsers() {
	f, err := os.Open("allusers.json")
	if err != nil {
		l.Println(err)
		return
	}
	allUsersMutex.Lock()
	json.NewDecoder(f).Decode(&allUsers)
	allUsersMutex.Unlock()
	f.Close()

	f, err = os.Open("bans.json")
	if err != nil {
		l.Println(err)
		return
	}
	bansMutex.Lock()
	json.NewDecoder(f).Decode(&bans)
	bansMutex.Unlock()
	f.Close()
}

func getMsgsFromSlack() {
	go rtm.ManageConnection()
	for msg := range rtm.IncomingEvents {
		switch ev := msg.Data.(type) {
		case *slack.MessageEvent:
			msg := ev.Msg
			if msg.SubType != "" {
				break // We're only handling normal messages.
			}
			u, _ := api.GetUserInfo(msg.User)
			if !strings.HasPrefix(msg.Text, "hide") {
				//h := sha1.Sum([]byte(msg.User))
				i, _ := binary.Varint([]byte(msg.User))

				broadcast(color.HiYellowString("HC ")+(*colorArr[rand.New(rand.NewSource(i)).Intn(len(colorArr))]).Sprint(strings.Fields(u.RealName)[0]), msg.Text, false)
			}
		case *slack.ConnectedEvent:
			fmt.Println("Connected to Slack")
		case *slack.InvalidAuthEvent:
			fmt.Println("Invalid token")
			return
		}
	}
}

func getSendToSlackChan() chan string {
	msgs := make(chan string, 100)
	go func() {
		for msg := range msgs {
			//if strings.HasPrefix(msg, "HC: ") { // just in case
			//	continue
			//}
			msg = strings.ReplaceAll(stripansi.Strip(msg), `\n`, "\n")
			rtm.SendMessage(rtm.NewOutgoingMessage(msg, "C01T5J557AA"))
		}
	}()
	return msgs
}

func findUserByName(name string) (*user, bool) {
	usersMutex.Lock()
	defer usersMutex.Unlock()
	for _, u := range users {
		if stripansi.Strip(u.name) == name {
			return u, true
		}
	}
	return nil, false
}

func remove(s []*user, a *user) []*user {
	var i int
	for i = range s {
		if s[i] == a {
			break // i is now where it is
		}
	}
	if i == 0 {
		return make([]*user, 0)
	}
	return append(s[:i], s[i+1:]...)
}
