# InfiniteStreamer.com Homepage Deployment

This directory contains **two deployment options** for the infinitestreamer.com homepage:

1. **WordPress** - Full CMS with easy content editing (recommended for frequent updates)
2. **Static HTML** - Fast, simple, secure (recommended for stability)

Both options are designed to run on your k3s server (lenovo) and include all content from the issue requirements.

## Quick Decision Guide

**Choose WordPress if:**
- ✅ You want a user-friendly admin interface
- ✅ Multiple people will update content
- ✅ You need frequent content updates
- ✅ You want plugins and themes

**Choose Static HTML if:**
- ✅ Content rarely changes
- ✅ You prefer maximum speed and security
- ✅ You want simplest possible setup
- ✅ You're comfortable editing HTML directly

---

# Option 1: WordPress Deployment

Full-featured CMS with admin panel for easy content management.

## WordPress Quick Start

**Ports:** WordPress on 30080, MySQL on 3306 (internal)

**Files:** `k8s-wordpress.yaml`, `deploy.sh`, `QUICKSTART.md`

### Prerequisites

- k3s cluster running on lenovo
- kubectl configured to access your k3s cluster
- Host directories for persistent storage (created automatically by deploy script)

### Deploy WordPress

**Using the Deploy Script (Recommended):**

```bash
cd wordpress-content
./deploy.sh
```

**Or Manually:**

```bash
# On lenovo, create directories
sudo mkdir -p /home/jonathanoliver/wordpress/{mysql,wp-content}
sudo chown -R $(id -u):$(id -g) /home/jonathanoliver/wordpress

# Deploy from your machine
kubectl apply -f k8s-wordpress.yaml

# Wait for pods
kubectl get pods -w
```

Access WordPress at: **http://lenovo.local:30080**

📚 **Full Instructions:** See [QUICKSTART.md](QUICKSTART.md) for complete setup guide

---

# Option 2: Static HTML Deployment

Fast, lightweight static site with no database required.

**Port:** 30082

**Files:** `static-homepage/index.html`, `static-homepage/k8s-static-site.yaml`

## Static Site Quick Start

### Deploy Static Site

```bash
kubectl apply -f static-homepage/k8s-static-site.yaml
```

Access site at: **http://lenovo.local:30082**

That's it! The static site is served from a ConfigMap (no persistent storage needed).

📚 **Full Instructions:** See [static-homepage/README.md](static-homepage/README.md)

---

# Files in This Directory

```
wordpress-content/
├── README.md                    # This file - overview of both options
├── QUICKSTART.md               # Detailed WordPress setup guide
├── deploy.sh                   # WordPress deployment script
├── homepage-content.html       # WordPress page content (copy/paste)
├── theme-style.css            # Optional custom CSS for WordPress
└── static-homepage/
    ├── README.md              # Static site documentation
    ├── index.html             # Static homepage (self-contained)
    └── k8s-static-site.yaml   # Static site k8s deployment
```

# Common Tasks

## Check Deployment Status

**WordPress:**
```bash
kubectl get pods -l app=wordpress
kubectl logs -f <wordpress-pod-name>
```

**Static:**
```bash
kubectl get pods -l app=infinitestreamer-static
```

## Update Content

**WordPress:**
- Log in to http://lenovo.local:30080/wp-admin
- Edit pages through the visual editor

**Static:**
- Edit `static-homepage/index.html`
- Redeploy: `kubectl delete -f static-homepage/k8s-static-site.yaml && kubectl apply -f static-homepage/k8s-static-site.yaml`

## Domain Setup (infinitestreamer.com)

### 1. Update DNS at GoDaddy
### 1. Update DNS at GoDaddy

- Log in to GoDaddy DNS Management
- Add A record: `@` → `[your lenovo public IP]`
- TTL: 600 seconds

### 2. Update Site URLs

**WordPress:**
```bash
kubectl exec -it <wordpress-pod> -- wp option update siteurl 'http://infinitestreamer.com' --allow-root
kubectl exec -it <wordpress-pod> -- wp option update home 'http://infinitestreamer.com' --allow-root
```

**Static:** No URL changes needed

### 3. Enable HTTPS (Optional but Recommended)

**Option A - Cloudflare (Easiest):**
1. Add infinitestreamer.com to Cloudflare (free)
2. Update nameservers at GoDaddy
3. Enable "Flexible SSL" in Cloudflare
4. Done! Cloudflare handles HTTPS

**Option B - cert-manager + Let's Encrypt:**
1. Install cert-manager in k3s
2. Create Ingress with TLS annotations
3. Let's Encrypt auto-provisions certificate

---

# Switching Between WordPress and Static

## From WordPress to Static
1. Deploy static site: `kubectl apply -f static-homepage/k8s-static-site.yaml`
2. Access on port 30082 first to verify
3. Remove WordPress: `kubectl delete -f k8s-wordpress.yaml`
4. Update DNS/ingress to point to static service

## From Static to WordPress
1. Deploy WordPress: `./deploy.sh` or `kubectl apply -f k8s-wordpress.yaml`
2. Complete WordPress setup
3. Access on port 30080 to verify
4. Remove static: `kubectl delete -f static-homepage/k8s-static-site.yaml`
5. Update DNS/ingress to point to WordPress service

---

# Troubleshooting

## WordPress Issues

**Can't access admin:**
- Check pod status: `kubectl get pods`
- Check logs: `kubectl logs <wordpress-pod>`
- Verify port 30080 is accessible

**Database connection error:**
- Wait for MySQL to fully start
- Check MySQL logs: `kubectl logs <mysql-pod>`

**Site loads slowly:**
- Install a caching plugin (WP Super Cache)
- Optimize images
- Consider using Cloudflare CDN

## Static Site Issues

**404 errors:**
- Verify ConfigMap: `kubectl get configmap infinitestreamer-static-html`
- Check pod logs: `kubectl logs <static-pod>`

**Content not updating:**
- Delete and recreate deployment:
  ```bash
  kubectl delete -f static-homepage/k8s-static-site.yaml
  kubectl apply -f static-homepage/k8s-static-site.yaml
  ```

---

# Backup and Recovery

## WordPress Backup

```bash
# Database
kubectl exec <mysql-pod> -- mysqldump -u wordpress -pWordPressPass123! wordpress > backup-$(date +%Y%m%d).sql

# Files
kubectl cp <wordpress-pod>:/var/www/html ./wordpress-files-backup
```

## Static Site Backup

Static site is in Git - no backup needed! Just redeploy from the yaml file.

---

# Performance Comparison

| Metric | WordPress | Static HTML |
|--------|-----------|-------------|
| Page Load | ~500ms-2s | ~50-200ms |
| Memory Usage | ~256MB | ~10MB |
| Setup Time | 10-15 min | 2-3 min |
| Update Time | 1-2 min | 5-10 min |
| Security Risk | Medium | Very Low |

---

# Next Steps

1. **Choose your deployment option** (WordPress or Static)
2. **Deploy to k3s** using instructions above
3. **Configure domain** at GoDaddy (optional)
4. **Enable HTTPS** via Cloudflare or cert-manager
5. **Test on multiple devices** (desktop, mobile, tablet)
6. **Set up monitoring** (uptime monitoring service)
7. **Schedule backups** (if using WordPress)

---

# Support and Resources

- **WordPress Setup:** See [QUICKSTART.md](QUICKSTART.md)
- **Static Site:** See [static-homepage/README.md](static-homepage/README.md)
- **Infinite Streaming:** https://github.com/jonathaneoliver/infinite-streaming
- **k3s Docs:** https://docs.k3s.io/
- **WordPress:** https://wordpress.org/documentation/

---

## Security Notes

**Important for Production:**

1. ⚠️ **Change default passwords** in `k8s-wordpress.yaml` before deploying
2. 🔐 Use **strong admin passwords** for WordPress
3. 🔒 Enable **HTTPS/SSL** for production use
4. 🔄 Keep WordPress/plugins **updated regularly**
5. 💾 Set up **automated backups**
6. 🛡️ Consider **security plugins** (Wordfence, iThemes Security)
7. 🚫 **Don't commit** real passwords to Git

---

*Created for the Infinite Streaming project homepage deployment*
