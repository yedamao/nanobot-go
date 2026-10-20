package cron

// Embed the zone database so job time zones resolve on hosts without
// one (minimal containers, Windows).
import _ "time/tzdata"
