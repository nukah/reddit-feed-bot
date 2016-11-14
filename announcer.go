package main

import (
  "os"
  "fmt"
  "log"
  "time"
  "regexp"
  "strconv"
  "strings"
  "net/http"
  "gopkg.in/redis.v5"
  "github.com/SlyMarbo/rss"
  "github.com/tucnak/telebot"
)

type FeedUpdate struct {
  item *rss.Item
  source string
}

var redditSubscribeRegexp = regexp.MustCompile(`^\/(subscribe)\s*\/r/(\w*)$`)
var redditUnsubscribeRegexp = regexp.MustCompile(`^\/(unsubscribe)\s*\/r/(\w*)$`)
var rClient = redis.NewClient(&redis.Options{ Addr: "redisdb:6379", DB: 0 })
var feedChan = make(chan FeedUpdate)

func refreshKey(name string) string {
  return fmt.Sprintf("refresh_time:%s", name)
}

func setUpdate(name string, updateTime time.Time) {
  rClient.Set(refreshKey(name), updateTime.Format(time.RFC1123Z), 0)
}

func fetchFunc(url string) (resp *http.Response, err error) {
  req, err := http.NewRequest("GET", url, nil)
  if err != nil {
    return
  }
  req.Header.Set("User-Agent", "MagpieRSS/0.7 ( http://magpierss.sf.net)")
  return http.DefaultClient.Do(req)
}

func needToUpdate(name string) bool {
  lastUpdate, err := rClient.Get(refreshKey(name)).Result()
  if err != nil || err == redis.Nil {
    return true
  }
  parsedTime, _ := time.Parse(time.RFC1123Z, lastUpdate)
  return time.Now().After(parsedTime)
}


func fetchUpdates(source string) {
  if needToUpdate(source) {
    feed, err := rss.FetchByFunc(fetchFunc, urlByKey(source))
    if err != nil {
      return
    }
    defer setUpdate(source, feed.Refresh)

    for _, item := range feed.Items {
      processItem(source, feed, item)
    }
  }
}

func processItem(source string, feed *rss.Feed, item *rss.Item) {
  if rClient.SIsMember("fetched_items", item.ID).Val() == true {
    return
  } else {
    if err := rClient.SAdd("fetched_items", item.ID).Err(); err == nil {
      feedChan <- FeedUpdate{item, source}
    }
  }
}

func addSourceToUser(source string, user_id int) {
  sourceKey := fmt.Sprintf("sources:%v", source)
  usersKey := fmt.Sprintf("users:%v", user_id)
  subscriptionStartKey := fmt.Sprintf("%s:subscribed_at", source)
  rPipe := rClient.Pipeline()
  defer rPipe.Close()
  defer rPipe.Exec()

  rPipe.HSet(subscriptionStartKey, strconv.FormatInt(int64(user_id), 10), time.Now().UTC().Format(time.RFC1123Z))
  rPipe.SAdd(sourceKey, user_id)
  rPipe.SAdd(usersKey, source)
  rPipe.SAdd("sources", source)
}

func removeSourceFromUser(source string, user_id int) {
  sourceKey := fmt.Sprintf("sources:%v", source)
  usersKey := fmt.Sprintf("users:%v", user_id)
  rPipe := rClient.Pipeline()
  defer rPipe.Close()
  defer rPipe.Exec()

  rPipe.SRem(sourceKey, user_id)
  rPipe.SRem(usersKey, source)
}

func removeSource(source string) {
  sourceKey := fmt.Sprintf("sources:%v", source)
  members, err := rClient.SMembers(sourceKey).Result()
  if err == nil && len(members) == 0 {
    rClient.SRem("sources", source)
  }
}

func urlByKey(source string) string {
  return fmt.Sprintf("%s.rss", sourceUrl(source))
}

func sourceUrl(source string) string {
  return fmt.Sprintf("https://www.reddit.com/r/%s", source)
}

func subscribeProcessor(bot *telebot.Bot, message telebot.Message) {
  if redditSubscribeRegexp.MatchString(message.Text) {
    sourceName := redditSubscribeRegexp.FindStringSubmatch(message.Text)[2]
    addSourceToUser(sourceName, message.Sender.ID)
    bot.SendMessage(message.Chat, fmt.Sprintf("Subscribing user %s to [/r/%s](%s)", message.Sender.FirstName, sourceName, urlByKey(sourceName)), &telebot.SendOptions{ParseMode: telebot.ModeMarkdown})
  }

  if redditUnsubscribeRegexp.MatchString(message.Text) {
    sourceName := redditUnsubscribeRegexp.FindStringSubmatch(message.Text)[2]
    removeSourceFromUser(sourceName, message.Sender.ID)
    removeSource(sourceName)
    bot.SendMessage(message.Chat, fmt.Sprintf("Unsubscribing user %s from [/r/%s](%s)", message.Sender.FirstName, sourceName, urlByKey(sourceName)), &telebot.SendOptions{ParseMode: telebot.ModeMarkdown})
  }

  if message.Text == "/list" {
    usersKey := fmt.Sprintf("users:%s", strconv.FormatInt(int64(message.Sender.ID), 10))
    userSubscriptions := rClient.SMembers(usersKey).Val()
    response := []string{}
    if len(userSubscriptions) == 0 {
      response = append(response, "You have no active subscriptions.")
    } else {
      response = append(response, "Your current subscriptions:")
      for _, subscription := range userSubscriptions {
        response = append(response, fmt.Sprintf("[%s](%s)", subscription, sourceUrl(subscription)))
      }
    }
    bot.SendMessage(message.Chat, strings.Join(response, "\n"), &telebot.SendOptions{ParseMode: telebot.ModeMarkdown})
  }
}

func renderUpdate(update FeedUpdate) string {
  template := "*/r/%s* @%s\n [%s](%s)"
  return fmt.Sprintf(template, update.source, update.item.Date.Format("2 Jan 2006: 15:04"), update.item.Title, update.item.Link)
}

func latestUpdate(user string, source string, updateTime time.Time) bool {
  subscriptionStartKey := fmt.Sprintf("%s:subscribed_at", source)
  subscribedAt := rClient.HGet(subscriptionStartKey, user).Val()
  if subscribedAt == "nil" {
    return false
  } else {
    subscribeTime, _ := time.Parse(time.RFC1123Z, subscribedAt)
    return updateTime.UTC().After(subscribeTime)
  }
}

func main() {
  messages := make(chan telebot.Message)
  ticker := time.NewTicker(time.Second * 5)
  go func() {
    for range ticker.C {
      for _, site := range rClient.SMembers("sources").Val() {
        go fetchUpdates(site)
      }
    }
  }()
  bot, err := telebot.NewBot(os.Getenv("API_TOKEN"))
  if err != nil {
    log.Fatalln(err)
  }
  bot.Listen(messages, 1 * time.Second)

  go func() {
    for feedUpdate := range feedChan {
      validUpdateRecievers := rClient.SMembers(fmt.Sprintf("sources:%v",feedUpdate.source)).Val()
      for _, user := range validUpdateRecievers {
        userID, err := strconv.Atoi(user)
        if err == nil && latestUpdate(user, feedUpdate.source, feedUpdate.item.Date) {
          bot.SendMessage(telebot.User{ID: userID}, renderUpdate(feedUpdate), &telebot.SendOptions{ParseMode: telebot.ModeMarkdown})
        }
      }
    }
  }()

  for message := range messages {
    subscribeProcessor(bot, message)
  }
}
