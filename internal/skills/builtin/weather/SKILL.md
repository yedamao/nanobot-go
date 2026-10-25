---
name: weather
description: Get current weather and forecasts (no API key needed).
---

# Weather

Two free services, no API keys needed. Use `web_fetch`, or `curl` through the exec tool.

## wttr.in (primary)

Pick the one request that matches the question; a today or forecast request already includes current conditions.

- Current conditions: `https://wttr.in/London?format=3` (e.g. `London: ⛅️ +8°C`)
- Custom format: `https://wttr.in/London?format=%l:+%c+%t+%h+%w`
- Today: `https://wttr.in/London?1&m`
- Full forecast: `https://wttr.in/London?T&m`

Format codes: `%c` condition · `%t` temp · `%h` humidity · `%w` wind · `%l` location · `%m` moon

Tips: URL-encode spaces (`New+York`); airport codes work (`JFK`); `?m` metric, `?u` USCS; `?0` current only, `?1` today only.

## Open-Meteo (fallback, JSON)

```
https://api.open-meteo.com/v1/forecast?latitude=51.5&longitude=-0.12&current_weather=true
```

Find the city's coordinates first. Returns JSON with temperature, wind speed and weather code. Docs: https://open-meteo.com/en/docs
