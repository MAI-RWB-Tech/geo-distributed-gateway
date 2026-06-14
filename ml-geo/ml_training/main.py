import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from src.training.trainer import train_pipeline
from src.config import DATA_RAW_DIR, FEATURE_CONFIG_PATH, MODEL_DIR


def main():
    parser = argparse.ArgumentParser(description="ML Training Service")
    parser.add_argument("command", choices=["train"], help="Command to run")
    parser.add_argument("--data-dir", default=DATA_RAW_DIR, help="Path to raw data")
    parser.add_argument("--config", default=FEATURE_CONFIG_PATH, help="Feature config path")
    parser.add_argument("--model-dir", default=MODEL_DIR, help="Output model directory")
    parser.add_argument("--params", default=None, help="JSON file with CatBoost params")
    parser.add_argument("--experiment", default="traffic_split_catboost", help="MLflow experiment name")

    args = parser.parse_args()

    params = None
    if args.params:
        with open(args.params) as f:
            params = json.load(f)

    if args.command == "train":
        model, metrics = train_pipeline(
            data_dir=args.data_dir,
            feature_config_path=args.config,
            model_dir=args.model_dir,
            params=params,
            experiment_name=args.experiment,
        )


if __name__ == "__main__":
    main()
