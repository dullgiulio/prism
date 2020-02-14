# Prism - Reverse proxy and traffic mirror

Prism reverse proxies one host and sends all the traffic to multiple mirrors.

## Usage

```
$ go install github.com/dullgiulio/prism
$ prism -proxy <upstream-host> -mirror <traffic-receiver1>[,<traffic-receiver2>,...] 
```

Additionally, `prism` can write full request and responses with mirrors to a file, by using the -dump option.

