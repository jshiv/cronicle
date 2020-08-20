This deployment setup describes how to standup a cronicle log server locally 
on top of the graphana/loki/vector stack
Setup a cronicle repo

_Build the cronicle binary via `go build -o deploy/local/`_
```
./cronicle init --path demo
./cronicle run --path demo/Cronicle.hcl
```
Install [vector](https://vector.dev/):

_Vector is the router that ships the cronicle logs to loki._
```
curl --proto '=https' --tlsv1.2 -sSf https://sh.vector.dev | sh
```
Pipe the cronicle logs from stdout to vector
```
./cronicle run --path demo/Cronicle.hcl | vector --config ./vector.toml
```
## Run [graphana](https://grafana.com/) and [loki](https://grafana.com/docs/loki/latest/overview/) with [docker](https://docs.docker.com/desktop/).
_
```
docker-compose up
```
Open http://localhost:3000/, username/password is admin/admin

Follow the [getting started instructions](https://grafana.com/docs/loki/latest/getting-started/grafana/) to explore the logs in graphana.

_Note: the loki server is http://loki:3100_

Try this sample query in the log explorer: `{key="cronicle", task="hello"}`