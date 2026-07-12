# 🧭 מצפן המניות — Stock Compass

אפליקציית ווב שמחשבת המלצות קנייה/מכירה למניות אמריקאיות (NYSE/NASDAQ)
לפי 8 אינדיקטורים טכניים מוכחים, ומאחדת אותם לציון אחד (0–100) והמלצה.

## ארכיטקטורה
- **Backend:** Go (ספריית סטנדרט, `net/http`), פורט `3003`. מחשב את כל האינדיקטורים בשרת.
- **Frontend:** אתר סטטי (`web/`) שקורא ל-`/api/analyze?symbol=X`.
- **נתונים:** Twelve Data (היסטוריה + מחיר), Finnhub (מחיר חי, רשות). המפתחות בשרת (k8s Secret), לא בדפדפן.

## מבנה
```
cmd/stockcompass/main.go        נקודת כניסה
internal/indicators/            מנוע האינדיקטורים
internal/marketdata/            לקוח Twelve Data + Finnhub
internal/server/                שרת HTTP + API
web/index.html                  הפרונט
Dockerfile                      multi-stage (golang:1.25-alpine -> alpine:3.19)
k8s/deployment.yaml             Deployment + Service + Ingress
.github/workflows/              בנייה ודחיפה אוטומטית ל-GHCR
```

## בנייה ופריסה
דחיפה ל-`main` בונה ודוחפת אוטומטית את האימג' ל-`ghcr.io/moshe1132767/stock-compass`.
אחר כך פריסה לקלאסטר:
```bash
kubectl set image deployment/stock-compass container-0=ghcr.io/moshe1132767/stock-compass:vX.Y
kubectl rollout status deployment/stock-compass
```

## הגדרת מפתח הנתונים
```bash
kubectl -n default create secret generic stock-compass-secrets \
  --from-literal=TWELVEDATA_API_KEY=<your-key> \
  --from-literal=FINNHUB_API_KEY=<optional>
```

> ניתוח טכני ממוחשב למטרות מידע והשכלה בלבד — לא ייעוץ השקעות.
