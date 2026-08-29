# CasaOS-MessageBus

[![Go Reference](https://pkg.go.dev/badge/github.com/IceWhaleTech/CasaOS-MessageBus.svg)](https://pkg.go.dev/github.com/IceWhaleTech/CasaOS-MessageBus) [![Go Report Card](https://goreportcard.com/badge/github.com/IceWhaleTech/CasaOS-MessageBus)](https://goreportcard.com/report/github.com/IceWhaleTech/CasaOS-MessageBus) [![goreleaser](https://github.com/IceWhaleTech/CasaOS-MessageBus/actions/workflows/release.yml/badge.svg)](https://github.com/IceWhaleTech/CasaOS-MessageBus/actions/workflows/release.yml) [![codecov](https://codecov.io/gh/IceWhaleTech/CasaOS-MessageBus/branch/main/graph/badge.svg?token=U4S4ZSZAL9)](https://codecov.io/gh/IceWhaleTech/CasaOS-MessageBus)

Message bus accepts events and actions from various sources and delivers them to subscribers.

See [openapi.yaml](./api/message_bus/openapi.yaml) for API specification.

## Liveness (operations)

`GET /v2/message_bus/health` answers the only question that matters during an
incident: **is the bus still delivering?** The unit staying `active` does not
prove it — the HTTP listeners keep the process alive even if a dispatcher
goroutine has died.

```bash
# as root on the box (no token needed: the socket lives in a 0700 dir)
curl --unix-socket /run/casanova/internal/message-bus.sock \
     http://unix/v2/message_bus/health
# through the gateway: same JWT as any other bus call
```

* HTTP **200** = every dispatcher running and accepting; **503** = something is
  wrong, and `problems` says what. The full snapshot is in the body either way.
* `goroutines[].restarts` / `restarts_total` are monotonic: a non-zero value
  means events *were* lost at some point, even if the bus is healthy now.
  `last_failure` keeps the reason of the most recent death.
* `events.loop_stale` is true when a dispatcher goroutine is alive but its
  select loop stopped iterating (it stamps a timestamp on every wake-up,
  including the idle 10s heartbeat).
* `subscribers: -1` means the count could not be read without blocking — the
  endpoint never waits on a dispatcher lock.

A dispatcher that dies is restarted automatically with exponential backoff (up
to 30s, never abandoned) and each restart is logged and pushed to systemd's
`STATUS=` line, so `systemctl status casanova-message-bus` shows the damage
after the fact.

Note that `POST /v2/message_bus/event/{source_id}/{name}` answers **503** when
the dispatcher does not take the event; a 200 means it was actually enqueued.

## publish api to npm

### edit version in package.json

### run
```bash
yarn

yarn start
```

### publish

Manual publish
```bash
yarn publish
```

Auto publish
```bash 
git push origin dev**
```