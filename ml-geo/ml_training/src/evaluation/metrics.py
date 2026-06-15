import numpy as np
from sklearn.metrics import mean_absolute_error, mean_squared_error, r2_score


def compute_metrics(model, X_test, y_test):
    y_pred = model.predict(X_test)
    y_pred = np.clip(y_pred, 0.0, 1.0)

    mae = mean_absolute_error(y_test, y_pred)
    rmse = np.sqrt(mean_squared_error(y_test, y_pred))
    r2 = r2_score(y_test, y_pred)

    return {
        "mae": float(mae),
        "rmse": float(rmse),
        "r2": float(r2),
        "predictions": y_pred,
        "actuals": y_test.values,
    }
