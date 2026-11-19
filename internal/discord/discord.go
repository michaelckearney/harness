// Package discord is the monorepo's Discord server, as harness serve uses
// it: messages it posts and keeps up to date in named channels, threads
// under them, and buttons people press.
package discord

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Message is what the service posts: an embed, the files shown with it,
// and a button.
type Message struct {
	Title       string
	URL         string
	Description string
	Color       int
	Files       []File
	Button      *Button
}

// File is attached to a message. An image shows under its caption, in an
// embed of its own.
type File struct {
	Name, ContentType string
	Data              []byte
	Caption           string
}

// maxImages is how many images show in embeds of their own: a message
// holds ten embeds, and the first is the message's. The rest are attached
// as they are.
const maxImages = 9

// captionLimit keeps a message's embeds within the 6000 characters Discord
// takes for all of them, with the first's description at Limit.
const captionLimit = 150

// Button is pressed by someone in the server; ID says what for.
type Button struct {
	ID, Label string
	Disabled  bool
}

// Press is the answer to a button: what the person who pressed it is told,
// which only they see.
type Press func(user, id string) string

// Msg is a message in the server.
type Msg struct {
	ID, Channel string
	Author      string // as the server shows them
	AuthorID    string
	Bot         bool // posted by a bot, this one or another
	Content     string
	Time        time.Time
	Files       []string // the URLs of its attachments
}

// Mention is a message that mentions the bot: in Thread, a thread, or else
// at the top of a channel, whose name is Where.
type Mention struct {
	Msg
	Thread bool
	Where  string
}

// Bot is the service's connection to one server.
type Bot struct {
	s        *discordgo.Session
	server   string
	channels map[string]string // by name
	names    map[string]string // by ID
}

// Open connects to the server, as the bot whose token it is, answers
// buttons with press, and hands each message that mentions it to mention.
func Open(token, server string, press Press, mention func(Mention)) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent
	b := &Bot{s: s, server: server, channels: map[string]string{}, names: map[string]string{}}
	s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.GuildID != server || m.Author == nil || m.Author.Bot || s.State.User == nil {
			return
		}
		mentioned := false
		for _, u := range m.Mentions {
			mentioned = mentioned || u.ID == s.State.User.ID
		}
		if !mentioned {
			return
		}
		ch, err := s.State.Channel(m.ChannelID)
		if err != nil {
			ch, err = s.Channel(m.ChannelID)
		}
		if err != nil {
			return
		}
		mention(Mention{Msg: b.msg(m.Message), Thread: ch.IsThread(), Where: ch.Name})
	})
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionMessageComponent || i.Member == nil {
			return
		}
		text := press(i.Member.User.ID, i.MessageComponentData().CustomID)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral},
		})
	})
	if err := s.Open(); err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	chans, err := s.GuildChannels(server)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("discord: the server's channels: %w", err)
	}
	for _, c := range chans {
		if c.Type == discordgo.ChannelTypeGuildText {
			b.channels[c.Name] = c.ID
			b.names[c.ID] = c.Name
		}
	}
	return b, nil
}

func (b *Bot) Close() { b.s.Close() }

func (b *Bot) channel(name string) (string, error) {
	if id := b.channels[name]; id != "" {
		return id, nil
	}
	return "", fmt.Errorf("discord: the server has no #%s", name)
}

func (m Message) embeds() []*discordgo.MessageEmbed {
	out := []*discordgo.MessageEmbed{{Title: m.Title, URL: m.URL, Description: m.Description, Color: m.Color}}
	for _, f := range m.Files {
		if len(out) > maxImages || !strings.HasPrefix(f.ContentType, "image/") {
			continue
		}
		caption := f.Caption
		if r := []rune(caption); len(r) > captionLimit {
			caption = string(r[:captionLimit-1]) + "…"
		}
		out = append(out, &discordgo.MessageEmbed{Title: caption, Color: m.Color,
			Image: &discordgo.MessageEmbedImage{URL: "attachment://" + f.Name}})
	}
	return out
}

func (m Message) components() []discordgo.MessageComponent {
	if m.Button == nil {
		return []discordgo.MessageComponent{}
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: m.Button.Label, CustomID: m.Button.ID, Style: discordgo.SuccessButton, Disabled: m.Button.Disabled},
	}}}
}

func (m Message) files() []*discordgo.File {
	var out []*discordgo.File
	for _, f := range m.Files {
		out = append(out, &discordgo.File{Name: f.Name, ContentType: f.ContentType, Reader: strings.NewReader(string(f.Data))})
	}
	return out
}

// Post posts a message in a channel, by name, and returns its ID.
func (b *Bot) Post(channel string, m Message) (string, error) {
	id, err := b.channel(channel)
	if err != nil {
		return "", err
	}
	msg, err := b.s.ChannelMessageSendComplex(id, &discordgo.MessageSend{
		Embeds: m.embeds(), Components: m.components(), Files: m.files(),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

// Edit replaces a message posted in a channel. Its files are replaced too,
// when files is true; otherwise they are left as they were.
func (b *Bot) Edit(channel, id string, m Message, files bool) error {
	ch, err := b.channel(channel)
	if err != nil {
		return err
	}
	embeds, components := m.embeds(), m.components()
	e := &discordgo.MessageEdit{ID: id, Channel: ch, Embeds: &embeds, Components: &components}
	if files {
		e.Attachments = &[]*discordgo.MessageAttachment{}
		e.Files = m.files()
	}
	_, err = b.s.ChannelMessageEditComplex(e)
	return err
}

// Thread starts a thread under a message posted in a channel, and returns
// its ID.
func (b *Bot) Thread(channel, message, name string) (string, error) {
	ch, err := b.channel(channel)
	if err != nil {
		return "", err
	}
	if len(name) > 100 {
		name = name[:100]
	}
	t, err := b.s.MessageThreadStart(ch, message, name, 10080)
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

// Say posts text in a thread, by its ID, in as many messages as it takes,
// with files after it. Users it mentions are pinged.
func (b *Bot) Say(thread, text string, files ...File) error {
	parts := Split(text, 2000, 2000)
	for i, part := range parts {
		send := &discordgo.MessageSend{Content: part,
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers}}}
		if i == len(parts)-1 {
			send.Files = Message{Files: files}.files()
		}
		if _, err := b.s.ChannelMessageSendComplex(thread, send); err != nil {
			return err
		}
	}
	return nil
}

// Last is when the newest message in a channel was posted, or the zero
// time if it has none.
func (b *Bot) Last(channel string) (time.Time, error) {
	ch, err := b.channel(channel)
	if err != nil {
		return time.Time{}, err
	}
	msgs, err := b.s.ChannelMessages(ch, 1, "", "", "")
	if err != nil || len(msgs) == 0 {
		return time.Time{}, err
	}
	return msgs[0].Timestamp, nil
}

func (b *Bot) msg(m *discordgo.Message) Msg {
	out := Msg{ID: m.ID, Channel: m.ChannelID, Content: m.Content, Time: m.Timestamp}
	if m.Author != nil {
		out.Author, out.AuthorID, out.Bot = m.Author.Username, m.Author.ID, m.Author.Bot
		if m.Author.GlobalName != "" {
			out.Author = m.Author.GlobalName
		}
	}
	if m.Member != nil && m.Member.Nick != "" {
		out.Author = m.Member.Nick
	}
	for _, a := range m.Attachments {
		out.Files = append(out.Files, a.URL)
	}
	// Mentions read as names, not numbers.
	for _, u := range m.Mentions {
		out.Content = strings.NewReplacer("<@"+u.ID+">", "@"+u.Username, "<@!"+u.ID+">", "@"+u.Username).Replace(out.Content)
	}
	return out
}

// Me is the bot's own user ID.
func (b *Bot) Me() string {
	if b.s.State.User == nil {
		return ""
	}
	return b.s.State.User.ID
}

// ThreadFrom starts a thread under a message, in a channel by its ID, and
// returns the thread's ID.
func (b *Bot) ThreadFrom(channel, message, name string) (string, error) {
	if len(name) > 100 {
		name = name[:100]
	}
	t, err := b.s.MessageThreadStart(channel, message, name, 10080)
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

// History is up to limit messages in a channel or thread, by its ID, the
// oldest first. A thread's starting message, in its channel, comes first.
func (b *Bot) History(channel string, limit int) ([]Msg, error) {
	var all []*discordgo.Message
	before := ""
	for len(all) < limit {
		n := min(100, limit-len(all))
		page, err := b.s.ChannelMessages(channel, n, before, "", "")
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < n {
			break
		}
		before = page[len(page)-1].ID
	}
	var out []Msg
	if ch, err := b.s.Channel(channel); err == nil && ch.IsThread() {
		if start, err := b.s.ChannelMessage(ch.ParentID, ch.ID); err == nil {
			out = append(out, b.msg(start))
		}
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Type == discordgo.MessageTypeDefault || all[i].Type == discordgo.MessageTypeReply {
			out = append(out, b.msg(all[i]))
		}
	}
	return out, nil
}

// Typing shows the bot typing in a channel or thread, for ten seconds.
func (b *Bot) Typing(channel string) { b.s.ChannelTyping(channel) }

// Link is a message's link, in a channel by its name.
func (b *Bot) Link(channel, message string) string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", b.server, b.channels[channel], message)
}

// Search finds messages in the server that match words, the most relevant
// first, with each one's link. Discord indexes a server on its first
// search, and asks to be asked again: Search waits for it.
func (b *Bot) Search(words string) ([]Msg, error) {
	u := discordgo.EndpointGuild(b.server) + "/messages/search?sort_by=relevance&limit=25&content=" + url.QueryEscape(words)
	for range 10 {
		data, err := b.s.RequestWithBucketID("GET", u, nil, discordgo.EndpointGuild(b.server)+"/messages/search")
		if err != nil {
			return nil, fmt.Errorf("discord: search: %w", err)
		}
		var r struct {
			Messages   [][]*discordgo.Message `json:"messages"`
			RetryAfter float64                `json:"retry_after"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		if r.Messages == nil && r.RetryAfter > 0 {
			time.Sleep(time.Duration(r.RetryAfter*float64(time.Second)) + time.Second)
			continue
		}
		var out []Msg
		for _, group := range r.Messages {
			if len(group) > 0 {
				out = append(out, b.msg(group[0]))
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("discord: the server's search index is not ready yet")
}

var linkRe = regexp.MustCompile(`^https://(?:\w+\.)?discord(?:app)?\.com/channels/(\d+)/(\d+)(?:/(\d+))?$`)

// Read is what a link to a message or a channel in the server shows: the
// message and those around it, or the channel's latest, the oldest first.
// A message that started a thread brings the thread with it.
func (b *Bot) Read(link string) ([]Msg, error) {
	m := linkRe.FindStringSubmatch(strings.TrimSpace(link))
	if m == nil || m[1] != b.server {
		return nil, fmt.Errorf("that is not a link to a message in this server")
	}
	channel, message := m[2], m[3]
	if message == "" {
		return b.History(channel, 50)
	}
	around, err := b.s.ChannelMessages(channel, 21, "", "", message)
	if err != nil {
		return nil, err
	}
	var out []Msg
	for i := len(around) - 1; i >= 0; i-- {
		out = append(out, b.msg(around[i]))
	}
	// A thread started from the message has the message's ID.
	if thread, err := b.History(message, 100); err == nil {
		out = append(out, thread...)
	}
	return out, nil
}

// Name is a channel's name, by its ID, for those the bot found when it
// connected.
func (b *Bot) Name(channel string) string { return b.names[channel] }
