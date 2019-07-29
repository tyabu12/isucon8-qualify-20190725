APP_NAME = torb.go
APP_SOURCE_DIR = /home/isucon/torb/webapp/go

WEB_NAME = h2o
WEB_ACCESS_LOG = /var/log/${WEB_NAME}/access.log
WEB_ERROR_LOG = /var/log/${WEB_NAME}/error.log

DB_NAME = mariadb
DB_USERNAME = isucon
DB_LOG = /var/log/${DB_NAME}/${DB_NAME}.log
DB_ERROR_LOG = /var/log/${DB_NAME}/error.log
DB_SLOW_LOG = /var/log/${DB_NAME}/slow.log


.DEFAULT_GOAL := help

service-status: ## Show status of services
	sudo systemctl list-unit-files -t service

restart: ## Restart web server and app services
	make -s web-restart app-restart

sync: ## Sync
	git fetch -pq \
	&& git reset --hard origin/master \
	&& make -s app-restart \
	&& make -s web-restart \

clean-log: ## Clean log files
	sudo truncate --size 0 ${WEB_ACCESS_LOG} ${DB_LOG} ${DB_ERROR_LOG} ${DB_SLOW_LOG}

app-restart: ## Restart app Service
	make -C ${APP_SOURCE_DIR} \
	&& sudo systemctl daemon-reload \
	&& sudo systemctl restart ${APP_NAME}

app-log: ## Show app log
	sudo journalctl -u ${APP_NAME}

web-restart: ## Restart web server
	sudo ${WEB_NAME} -t \
	&& sudo systemctl restart ${WEB_NAME}

web-log: ## Show web server access log
	sudo less +F ${WEB_ACCESS_LOG}

web-log-alp: ## Show alp [arg=<alp option>]
	sudo alp ${arg} -f ${WEB_ACCESS_LOG}

web-error: ## Show web server error log
	sudo less +F ${WEB_ERROR_LOG}

db-restart: ## Restart DB
	sudo cp etc/my.cnf /etc/my.cnf
	sudo cp etc/my.cnf.d/* /etc/my.cnf.d/
	sudo systemctl restart ${DB_NAME}

db-log: ## Show DB log
	sudo less +F ${DB_LOG}

db-error: ## Show DB error log
	sudo less +F ${DB_ERROR_LOG}

db-slow: ## Show DB slow log
	sudo pt-query-digest ${DB_SLOW_LOG}

.PHONY: help
help:
	@echo 'Usage: make <command> [arg=<arguments>]'
	@grep -E '^[a-z0-9A-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
