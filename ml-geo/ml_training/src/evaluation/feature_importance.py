import os
import numpy as np


def log_feature_importance(model, X_train, mlflow_run=True, output_dir=None):
    feature_names = list(X_train.columns)

    importance_arr = model.get_feature_importance()
    sorted_idx = np.argsort(importance_arr)[::-1]
    result = []
    for idx in sorted_idx:
        result.append({
            "feature": feature_names[idx],
            "importance": float(importance_arr[idx]),
        })

    if mlflow_run:
        import mlflow
        for item in result[:20]:
            mlflow.log_metric(f"importance_{item['feature']}", item["importance"])

    if output_dir:
        os.makedirs(output_dir, exist_ok=True)
        import json
        path = os.path.join(output_dir, "feature_importance.json")
        with open(path, "w") as f:
            json.dump(result, f, indent=2)

    print("  Top-10 features:")
    for item in result[:10]:
        print(f"    {item['feature']}: {item['importance']:.4f}")

    return result
