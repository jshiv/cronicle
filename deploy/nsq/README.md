# Cronicle on [NSQ](https://nsq.io/)

### Running in distributed mode with nsq as the message broker

1) Follow the nsq [quick-start guide](https://nsq.io/overview/quick_start.html) to get nsqlookupd,  nsqadmin and nsqd running.
__Note: nsqd should be running on the cronicle scheduler node__
```
nsqlookupd
nsqd --lookupd-tcp-address=127.0.0.1:4160
nsqadmin --lookupd-http-address=127.0.0.1:4161
```

2) Start a cronicle worker to consume from nsqlookup
__Note: 127.0.0.1:4161 is the nsqlookupd host, from a remote machine update to the hosts ip__
__Note: when --addr is specified, the cronicle worker consumes from nsqlookupd so a coloated nsqd is not required__
```
mkdir worker
cronicle worker  --path ./worker/ --queue nsq --addr 127.0.0.1:4161
```

3) Start the cronicle scheduler on a node with nsqd running.
`nsqd --lookupd-tcp-address=<nsqlookupd_host>:4160 --broadcast-address=<nsqd_host>`
`--addr <nsqlookupd_host>:4161`
```
nsqd --lookupd-tcp-address=127.0.0.1:4160 --broadcast-address=127.0.0.1
./cronicle init --path cron
cronicle run --path ./cron/cronicle.hcl --worker=true --queue nsq --addr 127.0.0.1:4161
```
