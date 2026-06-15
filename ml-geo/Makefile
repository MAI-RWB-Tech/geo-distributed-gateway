.PHONY: train inference test clean help

help:
	@echo "ML-Geo Load Balancer"
	@echo ""
	@echo "Commands:"
	@echo "  make train       - Train CatBoost model on CSV data"
	@echo "  make inference   - Start inference API server"
	@echo "  make docker-up   - Start inference service via Docker"
	@echo "  make docker-train- Run training in Docker"
	@echo "  make test        - Run all tests"
	@echo "  make clean       - Remove generated files"

train:
	cd ml_training && python main.py train --data-dir ../data/raw --config ../shared/feature_config.yaml --model-dir ../models/latest

inference:
	cd ml_inference && python main.py

docker-up:
	docker compose up ml-inference -d

docker-down:
	docker compose down

docker-train:
	docker compose --profile training run ml-training train

test:
	cd ml_training && python -m pytest tests/ -v
	cd ml_inference && python -m pytest tests/ -v

clean:
	rm -rf models/latest/*.cbm models/latest/*.json
	rm -rf data/processed/*
	rm -rf mlruns/ .mlflow/
