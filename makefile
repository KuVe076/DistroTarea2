docker-lcp:
	sudo docker-compose down
	sudo docker-compose up --build lcp

docker-entrenadores-cdp:
	sudo docker-compose down
	sudo docker-compose build entrenador cdp
	sudo docker-compose up -d cdp
	sudo docker-compose run --rm entrenador 

docker-gimnasio:
	sudo docker-compose down
	sudo docker-compose up --build gimnasios

docker-snp:
	sudo docker-compose down
	sudo docker-compose up --build snp

