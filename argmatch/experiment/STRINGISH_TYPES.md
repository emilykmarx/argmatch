Types in the Prometheus config that are not obviously strings:



1. Converted to a number: the original text is gone

model.Duration (scrape_interval, refresh_interval, …): ParseDuration(s) produces an int64 count of nanoseconds. The original spelling isn't kept, so "300s" and "5m" decode to the same value, and printing it gives the canonical form (5m).
config.TLSVersion (tls_config.min_version/max_version): "TLS12" is looked up in a table and stored as a uint16.
units.Base2Bytes (body_size_limit): "512MB" is stored as an int64 byte count.
2. Parsed into a structure: the text is split into pieces

config.URL (url, proxy_url, api_server, …): url.Parse(s) stores a *url.URL, i.e. scheme, host, path, query and so on as separate fields. Most of the original text survives as substrings, and .String() usually rebuilds the same URL. But it isn't byte-for-byte guaranteed: url.Parse normalizes some things, such as making the scheme lowercase.
3. Kept word for word, wrapped

relabel.Regexp (regex): it compiles "^(?s:" + s + ")$". The compiled regexp keeps that expression text, so the original string is inside it unchanged. Regexp.String() strips the added wrapper and returns exactly the original.
4. Still a string

relabel.Action (action): stays a string but is lowercased (strings.ToLower) and validated, so "Replace" becomes "replace".
config.Secret: type plain Secret decoding, so the string is unchanged.
The Role types, LabelName and others classified scalar via type plain T: these are string types decoded as plain strings, so presumably unchanged. I haven't checked each one for validation or normalization.
