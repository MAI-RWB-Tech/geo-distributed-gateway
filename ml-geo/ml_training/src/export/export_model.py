import os


def export_model(model, model_dir: str, version: str, feature_columns: list):
    import json

    model_path = os.path.join(model_dir, f"model_{version}.cbm")
    model.model.save_model(model_path)

    columns_path = os.path.join(model_dir, f"model_{version}_columns.json")
    with open(columns_path, "w") as f:
        json.dump(feature_columns, f)

    print(f"  Model saved: {model_path}")
    print(f"  Columns saved: {columns_path}")
    return model_path
