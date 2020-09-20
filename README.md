# TwitterPub

An ActivityPub gateway for Twitter.

Currently features:

- Profiles
- Tweets and conversations, including emojis, links, mentions ~~and images~~
- Redirections to Twitter for browsers
- A minimal web interface to generate new URLs

As a Mastodon user (or anything ActivityPub-compatible), it mostly means you can:

- Easily refer to tweets by replacing the domain name
- Open those tweet URLs in Mastodon's search box and make it pull the tweets and profiles
- Boost those tweets as any toot, which is much better than quoting and using "@twitter.com" unless they support ActivityPub.
  It will have a correct timestamp, the right user image and profile, and a proper link back to it.


Technically, it's a Go http server that shows endpoints mirroring Twitter's URLs.
Depending on the Accept header, it will redirect requests to Twitter or
serve an ActivityPub server-to-server API.
Everything is currently fetched by parsing the HTML of Twitter's main site,
as it doesn't require authentication and doesn't rate-limit as aggressively
as the API, which is a sad state for a modern service.
