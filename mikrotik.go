package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/crowdsecurity/crowdsec/pkg/models"
	"github.com/jellydator/ttlcache/v3"
	"github.com/rs/zerolog/log"

	"github.com/go-routeros/routeros/v3"
)

func dial() (*routeros.Client, error) {
	if useTLS {
		return routeros.DialTLSTimeout(mikrotikHost, username, password, nil, timeout)
	}
	return routeros.DialTimeout(mikrotikHost, username, password, timeout)
}

// runMikrotikCommands is a loop which walks over the cached address list
// and adds addresses to ne address-list  in mikrotik
// and updates firewall rules to use that new address list
//
// we need it to be executed periodically to ensure that if we use default_ttl_max
// then we readd address prior expiry
func runMikrotikCommands(mal *mikrotikAddrList) {
	go func() {
		for {

			// on app start and cache items is empty, wait 10 seconds for an update
			// usually decisions are initiated under 5s on initial connect
			if len(mal.cache.Items()) == 0 {
				time.Sleep(10 * time.Second)
			}

			// TODO: allow defining custom format of target address-list name
			listName := fmt.Sprintf("%s_%s", addressList, time.Now().UTC().Format("2006-01-02_15-04-05"))
			var err error
			var conn *routeros.Client
			conn, err = mikrotikConnect()
			if err != nil {
				return
			}
			mal.c = conn
			defer mikrotikClose(mal.c)
			for _, item := range mal.cache.Items() {
				address := item.Key()
				ttl := item.TTL()
				comment := item.Value()
				err := mal.addToAddressList(listName, address, ttl, comment)
				if err != nil {
					return
				}
			}

			if useIPV4 {
				err = mal.setAddressListInFilter("ip", listName, firewallRuleIdsIPv4)

			} else {
				log.Debug().
					Str("func", "runMikrotikCommands").
					Str("list_name", listName).
					Msgf("Skipping setAddressListInFilter, because IPv4 support is disabled")
			}

			if useIPV6 {
				err = mal.setAddressListInFilter("ipv6", listName, firewallRuleIdsIPv6)
			} else {
				log.Debug().
					Str("func", "runMikrotikCommands").
					Str("list_name", listName).
					Msgf("Skipping setAddressListInFilter, because IPv6 support is disabled")
			}

			time.Sleep(updateFreq)

		}
	}()
}

func mikrotikConnect() (*routeros.Client, error) {

	log.Info().
		Str("func", "mikrotikConnect").
		Str("host", mikrotikHost).
		Str("username", username).
		Bool("useTLS", useTLS).
		Str("timeout", timeout.String()).
		Msg("Connecting to mikrotik")

	c, err := dial()
	if err != nil {
		log.Error().
			Err(err).
			Str("func", "connect").
			Str("host", mikrotikHost).
			Str("username", username).
			Bool("useTLS", useTLS).
			Str("timeout", timeout.String()).
			Msg("Connecting to mikrotik failed")
		return nil, err
	}

	return c, nil

}

func mikrotikClose(c *routeros.Client) error {

	log.Info().
		Str("func", "mikrotikClose").
		Str("host", mikrotikHost).
		Str("username", username).
		Bool("useTLS", useTLS).
		Str("timeout", timeout.String()).
		Msg("Closing connection to mikrotik")

	err := c.Close()
	if err != nil {
		log.Error().
			Err(err).
			Str("func", "mikrotikClose").
			Str("host", mikrotikHost).
			Str("username", username).
			Bool("useTLS", useTLS).
			Str("timeout", timeout.String()).
			Msg("Closing connection to mikrotik failed.")
		return err
	}
	return nil
}

func (mal *mikrotikAddrList) add(decision *models.Decision) {

	log.Info().
		Str("func", "add").
		Str("duration", *decision.Duration).
		Str("origin", *decision.Origin).
		Str("scenario", *decision.Scenario).
		Str("scope", *decision.Scope).
		// Bool("simulated", *decision.Simulated).
		// Int64("id", decision.ID).
		// Str("type", *decision.Type).
		// Str("until", decision.Until).
		// Str("uuid", decision.UUID).
		Str("value", *decision.Value).
		Msg("Processing new decision to add")

	address := *decision.Value
	newTTL := setTTL(*decision.Duration)
	proto := getProtoCmd(address)

	if proto == "ip" && !useIPV4 {
		log.Debug().
			Str("func", "add").
			Str("address", address).
			Str("new_ttl", newTTL.String()).
			Msg("skipping, IPv4 not enabled")
		metricDecision.WithLabelValues(proto, "add", "skip").Inc()
		return
	}

	if proto == "ipv6" && !useIPV6 {
		log.Debug().
			Str("func", "add").
			Str("address", address).
			Str("new_ttl", newTTL.String()).
			Msg("skipping, IPv6 not enabled")
		metricDecision.WithLabelValues(proto, "add", "skip").Inc()
		return
	}

	if proto == "ipv6" && useIPV6 {
		address += "/128"
	}

	// TODO: allow formatting comment for decision
	comment := fmt.Sprintf("%s %s %s", *decision.Origin, *decision.Scenario, *decision.Scope)

	var item = &ttlcache.Item[string, string]{}

	if mal.cache.Has(address) {
		metricCache.WithLabelValues("add", "hit").Inc()
		item = mal.cache.Get(address)
		currentTTL := time.Until(item.ExpiresAt())

		switch {
		case newTTL == currentTTL:
			metricDecision.WithLabelValues(proto, "add", "update_equal").Inc()
		case newTTL > currentTTL:
			metricDecision.WithLabelValues(proto, "add", "update_extend").Inc()
		case newTTL < currentTTL:
			metricDecision.WithLabelValues(proto, "add", "update_shorten").Inc()
		}
		log.Info().
			Str("func", "add").
			Str("address", address).
			Str("current_ttl", currentTTL.String()).
			Str("new_ttl", newTTL.String()).
			Msg("Address is in the cache, updating")

	} else {
		metricCache.WithLabelValues("add", "miss").Inc()
		log.Info().
			Str("func", "add").
			Str("address", address).
			Str("new_ttl", newTTL.String()).
			Msg("Address not in cache, adding")
		metricDecision.WithLabelValues(proto, "add", "insert").Inc()
	}

	mal.cache.Set(address, comment, newTTL)
}

func (mal *mikrotikAddrList) remove(decision *models.Decision) {

	log.Info().
		Str("func", "add").
		Str("duration", *decision.Duration).
		Str("origin", *decision.Origin).
		Str("scenario", *decision.Scenario).
		Str("scope", *decision.Scope).
		// Bool("simulated", *decision.Simulated).
		// Int64("id", decision.ID).
		// Str("type", *decision.Type).
		// Str("until", decision.Until).
		// Str("uuid", decision.UUID).
		Str("value", *decision.Value).
		Msg("Processing new decision to remove")

	proto := getProtoCmd(*decision.Value)
	address := *decision.Value
	newTTL := setTTL(*decision.Duration)
	if proto == "ip" && !useIPV4 {
		log.Debug().
			Str("func", "add").
			Str("address", address).
			Str("new_ttl", newTTL.String()).
			Msg("skipping, IPv4 not enabled")
		metricDecision.WithLabelValues(proto, "remove", "skip").Inc()
		return
	}

	if proto == "ipv6" && !useIPV6 {
		log.Debug().
			Str("func", "add").
			Str("address", address).
			Str("new_ttl", newTTL.String()).
			Msg("skipping, IPv6 not enabled")
		metricDecision.WithLabelValues(proto, "remove", "skip").Inc()
		return
	}

	var item = &ttlcache.Item[string, string]{}

	if mal.cache.Has(address) {
		metricCache.WithLabelValues("del", "hit").Inc()
		item = mal.cache.Get(address)
		currentTTL := time.Until(item.ExpiresAt())
		log.Info().
			Str("func", "remove").
			Str("address", address).
			Str("ttl", currentTTL.String()).
			Msgf("Address is in the cache, removing")
		metricDecision.WithLabelValues(proto, "remove", "remove").Inc()
		mal.cache.Delete(address)

	} else {
		log.Info().
			Str("func", "remove").
			Str("address", address).
			Str("new_ttl", newTTL.String()).
			Msg("Address not in cache, nothing to do")

		metricCache.WithLabelValues("del", "miss").Inc()
		metricDecision.WithLabelValues(proto, "remove", "no_op").Inc()
	}

}

// decisionProcess runs in a loop every 5 seconds by default
//
// so if there are some changes we will process them, such as add/remove
//
// then if there were any decisions there will be a trigger of swapping address-lists in firewall rules
// thus we create a new list and use it as new
// old rule should auto-expire so there is no need fo cleanups
func (mal *mikrotikAddrList) decisionProcess(streamDecision *models.DecisionsStreamResponse) {

	decisionsAdded := 0
	decisionsDeleted := 0

	for _, decision := range streamDecision.Deleted {
		mal.remove(decision)
		decisionsDeleted++
		if decisionsDeleted == debugDecisionsMax {
			break
		}
	}

	for _, decision := range streamDecision.New {
		mal.add(decision)
		decisionsAdded++
		if decisionsAdded == debugDecisionsMax {
			break
		}
	}

}

// setTTL parses input time string
// if it cannot parse it then it returns default cache duration and spews warning to log
func setTTL(timeStr string) time.Duration {
	ttl, err := ParseMikrotikDuration(timeStr)
	if err != nil {
		ttl = defaultTTL
		log.Warn().Err(err).
			Str("func", "setTTL").
			Str("input", timeStr).
			Str("effective", ttl.String()).
			Msg("Failed to parse input mikrotik timeout value, setting default TTL")
	}
	// TODO: add metric for good/bad timeout values
	return ttl
}

// ParseMikrotikDuration parses a duration string,
// with addition of parsing days, weeks, and years.
//
// examples:
//
// "10d", "2w" or "3y4m5d".
func ParseMikrotikDuration(s string) (time.Duration, error) {
	neg := false
	if len(s) > 0 && s[0] == '-' {
		neg = true
		s = s[1:]
	}

	re := regexp.MustCompile(`(\d*\.\d+|\d+)[^\d]*`)
	unitMap := map[string]time.Duration{
		"d": 24,
		"w": 7 * 24,
		"y": 365 * 24,
	}

	strs := re.FindAllString(s, -1)
	var sumDur time.Duration
	for _, str := range strs {
		var _hours time.Duration = 1
		for unit, hours := range unitMap {
			if strings.Contains(str, unit) {
				str = strings.ReplaceAll(str, unit, "h")
				_hours = hours
				break
			}
		}

		dur, err := time.ParseDuration(str)
		if err != nil {
			return 0, err
		}

		sumDur += dur * _hours
	}

	if neg {
		sumDur = -sumDur
	}
	return sumDur, nil
}

// getProtoCmd returns protocol from address (but detection is pretty dumb)
//
// "ip" for IPv4
// "ipv6" for IPv6
//
// to be used by mirkotik api calls
func getProtoCmd(address string) string {
	if strings.Contains(address, ":") {
		return "ipv6"
	}
	return "ip"

}

// addToAddressList adds address to address-list
//
// listName - address-list-name
//
// address - address to add
// ttl - timeout for the address in the address-list
// comment
func (mal *mikrotikAddrList) addToAddressList(listName string, address string, ttl time.Duration, comment string) error {

	proto := getProtoCmd(address)
	if proto != "ip" && proto != "ipv6" {
		log.Error().
			Str("func", "setAddressListInFilter").
			Str("proto", proto).
			Str("listName", listName).
			Str("address", address).
			Str("ttl", ttl.String()).
			// Str("comment", comment).
			Msgf("Invalid protocol, valid values are 'ip' or 'ipv6'")
		return nil
	}

	if ttl == 0*time.Second {
		newTTL := 2 * updateFreq
		log.Info().
			Str("func", "addToAddressList").
			Str("ttl", ttl.String()).
			Str("ttl_updated", newTTL.String()).
			Msgf("Ban without TTL converted to expiring ban")
		metricPermBans.WithLabelValues(proto).Inc()
		ttl = newTTL
	}

	ttlTruncated := "false"
	if useMaxTTL && ttl > maxTTL {
		ttl = maxTTL
		ttlTruncated = "true"
	}
	metricTTLTruncated.WithLabelValues(proto, ttlTruncated).Inc()

	log.Debug().
		Str("func", "addToAddressList").
		Msgf("mikrotik: /%s firewall address-list add list=%s address=%s comment='%s' timeout=%s", proto, listName, address, comment, ttl)

	cmd := fmt.Sprintf("/%s/firewall/address-list/add#=list=%s#=address=%s#=comment=%s#=timeout=%s", proto, listName, address, comment, ttl)

	r, err := mal.c.RunArgs(strings.Split(cmd, "#"))
	log.Debug().
		Str("func", "addToAddressList").
		Msgf("response: '%v'", r)
	if err != nil {
		log.Error().Err(err).
			Str("func", "addToAddressList").
			Str("proto", proto).
			Str("list_name", listName).
			Str("address", address).
			Str("ttl", ttl.String()).
			Str("ttl_truncated", ttlTruncated).
			// Str("comment", comment).
			Msgf("Failed to add address to adress-list")
		metricMikrotikCmd.WithLabelValues(proto, "address_list", "add", "error").Inc()
		return err

	}
	metricMikrotikCmd.WithLabelValues(proto, "address_list", "add", "success").Inc()

	log.Info().
		Str("func", "addToAddressList").
		Str("proto", proto).
		Str("list_name", listName).
		Str("address", address).
		Str("ttl", ttl.String()).
		Str("ttl_truncated", ttlTruncated).
		// Str("comment", comment).
		Msgf("Address added to mikrotik successfully")
	return nil
}

// setAddressListInFilter sets given listName as src-address-list in firewall filter rule
//
// firewallRuleId - filter id in mikrotik firewall rules
//
// listName - name of the list in the address-list, we assume it exists
func (mal *mikrotikAddrList) setAddressListInFilter(proto string, listName string, firewallRuleIds string) error {

	if proto != "ip" && proto != "ipv6" {
		log.Error().
			Str("func", "setAddressListInFilter").
			Str("proto", proto).
			Str("listName", listName).
			Str("firewallRuleIds", firewallRuleIds).
			Msgf("Invalid protocol, valid values are 'ip' or 'ipv6'")
		return nil
	}

	log.Debug().
		Str("func", "setAddressListInFilter").
		Str("proto", proto).
		Str("src-address-list", listName).
		Str("number", firewallRuleIds).
		Msgf("mikrotik: /%s firewall filter set src-address-list=%s number=%s", proto, listName, firewallRuleIds)

	cmd := fmt.Sprintf("/%s/firewall/filter/set#=src-address-list=%s#=.id=%s", proto, listName, firewallRuleIds)

	r, err := mal.c.RunArgs(strings.Split(cmd, "#"))
	log.Debug().
		Str("func", "setAddressListInFilter").
		Msgf("response: '%v'", r)
	if err != nil {
		log.Error().Err(err).
			Str("func", "setAddressListInFilter").
			Str("proto", proto).
			Str("src-address-list", listName).
			Str("number", firewallRuleIds).
			Msgf("Failed to set address-list in filter")
		metricMikrotikCmd.WithLabelValues(proto, "filter", "set", "error").Inc()
		return err

	}
	metricMikrotikCmd.WithLabelValues(proto, "filter", "set", "success").Inc()
	log.Info().
		Str("func", "setAddressListInFilter").
		Str("proto", proto).
		Str("src-address-list", listName).
		Str("number", firewallRuleIds).
		Msgf("New address-list set to firewall filter in mikrotik successfully")
	return nil
}
