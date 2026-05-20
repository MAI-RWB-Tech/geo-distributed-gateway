import numpy as np
from sklearn.metrics import mean_absolute_error


def run_backtest(model, X, y, window_size=50):
    predictions = []
    actuals = []

    for i in range(0, len(X) - window_size, window_size):
        X_window = X.iloc[i : i + window_size]
        y_window = y.iloc[i : i + window_size]

        y_pred = model.predict(X_window)
        y_pred = np.clip(y_pred, 0.0, 1.0)

        predictions.extend(y_pred)
        actuals.extend(y_window.values)

    predictions = np.array(predictions)
    actuals = np.array(actuals)

    mae = mean_absolute_error(actuals, predictions)

    return {
        "mae": float(mae),
        "predictions": predictions,
        "actuals": actuals,
    }
