# Quick Start Guide - Infinite Streaming WordPress Homepage

## For k3s Deployment on Lenovo

### Prerequisites
- k3s installed and running on lenovo server
- kubectl configured to connect to your k3s cluster
- SSH access to lenovo server

### Step 1: Deploy to k3s

From your development machine (with kubectl access to lenovo):

```bash
cd /path/to/infinite-streaming
./wordpress-content/deploy.sh
```

Or manually:

```bash
# Create directories on lenovo
ssh lenovo "sudo mkdir -p /home/jonathanoliver/wordpress/{mysql,wp-content} && sudo chown -R \$(id -u):\$(id -g) /home/jonathanoliver/wordpress"

# Deploy from your machine
kubectl apply -f k8s-wordpress.yaml

# Wait for pods to be ready
kubectl get pods -w
```

### Step 2: Access WordPress

Open your browser:
- **Local network**: http://lenovo.local:30080
- **Direct IP**: http://192.168.0.189:30080

### Step 3: Complete WordPress Installation

1. Select language → Continue
2. Fill in site details:
   - Site Title: **Infinite Streaming**
   - Username: (your choice)
   - Password: (strong password)
   - Email: your-email@example.com
3. Click "Install WordPress"

### Step 4: Configure Homepage

#### Upload Images First
1. Go to **Media → Add New**
2. Upload all files from `screenshots/` directory
3. Note the URLs of uploaded images

#### Create Homepage
1. Go to **Pages → Add New**
2. Title: "Home"
3. Switch to "Text" (HTML) editor
4. Copy content from `wordpress-content/homepage-content.html`
5. Replace `[INSERT-*-IMAGE]` placeholders with actual image URLs
6. Click "Publish"

#### Set as Homepage
1. Go to **Settings → Reading**
2. Select "A static page"
3. Choose "Home" as Homepage
4. Save Changes

### Step 5: Optimize Appearance

#### Install a Lightweight Theme
1. Go to **Appearance → Themes → Add New**
2. Search for and install one of:
   - **Astra** (recommended - fast & customizable)
   - **GeneratePress** (lightweight & flexible)
   - **Kadence** (modern & responsive)
3. Activate the theme

#### Customize Theme
1. Go to **Appearance → Customize**
2. Adjust:
   - Colors (use #667eea, #764ba2 for gradient)
   - Typography (modern sans-serif)
   - Layout (full-width for homepage)
   - Header/Footer (minimal or hidden)

#### Add Custom CSS (Optional)
1. In Customizer, go to **Additional CSS**
2. Copy styles from `wordpress-content/theme-style.css`
3. Adjust as needed

### Step 6: Domain Configuration (infinitestreamer.com)

#### Update DNS at GoDaddy
1. Log in to GoDaddy
2. Go to DNS Management for infinitestreamer.com
3. Add A Record:
   - Type: A
   - Name: @
   - Value: [Your lenovo public IP]
   - TTL: 600 (or default)

#### Update WordPress URLs
```bash
# Get WordPress pod name
kubectl get pods -l app=wordpress,tier=frontend

# Update site URLs
kubectl exec -it <wordpress-pod-name> -- wp option update siteurl 'http://infinitestreamer.com' --allow-root
kubectl exec -it <wordpress-pod-name> -- wp option update home 'http://infinitestreamer.com' --allow-root
```

#### For HTTPS (Recommended)

**Option A: Using Cloudflare (Easiest)**
1. Sign up for Cloudflare (free tier)
2. Add infinitestreamer.com to Cloudflare
3. Update nameservers at GoDaddy to Cloudflare's
4. Enable "Flexible SSL" in Cloudflare
5. WordPress will be http, Cloudflare serves https to visitors

**Option B: Using k3s Ingress + Let's Encrypt**
1. Install cert-manager in k3s
2. Create Ingress resource with TLS
3. Let's Encrypt will auto-provision SSL certificate

Example Ingress (create as `wordpress-ingress.yaml`):
```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: wordpress-ingress
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
spec:
  tls:
  - hosts:
    - infinitestreamer.com
    secretName: wordpress-tls
  rules:
  - host: infinitestreamer.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: wordpress
            port:
              number: 80
```

Then:
```bash
kubectl apply -f wordpress-ingress.yaml
```

## Useful Commands

### Check Status
```bash
# Pod status
kubectl get pods

# Service info
kubectl get svc wordpress

# Logs
kubectl logs -f <wordpress-pod-name>
```

### Restart Services
```bash
kubectl rollout restart deployment wordpress
kubectl rollout restart deployment wordpress-mysql
```

### Backup
```bash
# Database
kubectl exec <mysql-pod-name> -- mysqldump -u wordpress -pWordPressPass123! wordpress > backup-$(date +%Y%m%d).sql

# Files
kubectl cp <wordpress-pod-name>:/var/www/html ./wordpress-backup-$(date +%Y%m%d)
```

### Update WordPress
```bash
# Access WordPress pod
kubectl exec -it <wordpress-pod-name> -- bash

# Inside pod
wp core update --allow-root
wp plugin update --all --allow-root
wp theme update --all --allow-root
```

### Remove Deployment
```bash
kubectl delete -f k8s-wordpress.yaml
# Data persists in /home/jonathanoliver/wordpress/
```

## Troubleshooting

### Can't access WordPress
- Check pods: `kubectl get pods`
- Check logs: `kubectl logs <wordpress-pod-name>`
- Verify port 30080 is open: `netstat -ln | grep 30080`

### Database connection error
- Wait for MySQL to fully start: `kubectl logs <mysql-pod-name>`
- Check secret: `kubectl get secret wordpress-mysql-secret -o yaml`

### Persistent volume issues
- Verify directories exist: `ls -la /home/jonathanoliver/wordpress/`
- Check permissions: `sudo chown -R $(id -u):$(id -g) /home/jonathanoliver/wordpress/`

### Site loads but looks broken
- Clear WordPress cache
- Check theme is activated
- Verify images are uploaded and URLs are correct

## Next Steps After Deployment

1. **Security Hardening**
   - Change default passwords
   - Install Wordfence or similar security plugin
   - Keep WordPress updated
   - Use strong admin password
   - Limit login attempts

2. **Performance Optimization**
   - Install caching plugin (WP Super Cache or W3 Total Cache)
   - Optimize images
   - Enable CDN if using Cloudflare

3. **SEO Setup**
   - Install Yoast SEO or Rank Math
   - Add meta descriptions
   - Submit sitemap to Google Search Console

4. **Analytics**
   - Add Google Analytics or similar
   - Set up uptime monitoring

5. **Backup Strategy**
   - Set up automated backups
   - Store backups offsite
   - Test restore process

## Support

- **Infinite Streaming**: https://github.com/jonathaneoliver/infinite-streaming
- **WordPress Docs**: https://wordpress.org/documentation/
- **k3s Docs**: https://docs.k3s.io/
- **kubectl Cheat Sheet**: https://kubernetes.io/docs/reference/kubectl/cheatsheet/
