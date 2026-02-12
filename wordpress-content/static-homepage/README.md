# Static HTML Homepage Alternative

This directory contains a static HTML version of the infinitestreamer.com homepage that can be deployed without WordPress.

## Benefits of Static Site

- **Faster**: No database queries, no PHP processing
- **Simpler**: Just HTML, CSS, and images
- **More Secure**: No CMS to maintain or update
- **Easier**: No WordPress setup needed

## Drawbacks

- **No CMS**: Must edit HTML directly to make changes
- **No Admin Panel**: No user-friendly editor
- **Manual Updates**: Content changes require file edits and redeployment

## Deployment Options

### Option 1: Deploy with nginx on k3s

Use the provided k8s manifest:

```bash
# Copy screenshots to the correct location
cp -r ../../screenshots ./

# Deploy to k3s
kubectl apply -f k8s-static-site.yaml

# Access at http://lenovo.local:30082
```

### Option 2: Deploy with Any Web Server

The static site is just HTML/CSS, so it can be served by any web server:

**Using nginx:**
```bash
# Copy files to nginx web root
sudo cp -r static-homepage /var/www/html/

# Access at http://your-server/static-homepage/
```

**Using Python:**
```bash
cd static-homepage
python3 -m http.server 8000
# Access at http://localhost:8000
```

**Using Node.js:**
```bash
npm install -g http-server
cd static-homepage
http-server -p 8000
# Access at http://localhost:8000
```

### Option 3: Deploy to GitHub Pages

1. Create a new repository or use existing
2. Copy contents of `static-homepage/` to root or docs folder
3. Enable GitHub Pages in repository settings
4. Access at https://username.github.io/repo-name/

### Option 4: Deploy to Netlify/Vercel (Free)

**Netlify:**
1. Sign up at https://netlify.com
2. Drag and drop the `static-homepage` folder
3. Get instant https://your-site.netlify.app domain
4. Can configure custom domain

**Vercel:**
1. Sign up at https://vercel.com
2. Import from Git or drag folder
3. Get instant https://your-site.vercel.app domain

## Making Changes

### Update Text Content

Edit `index.html` and modify the HTML content:

```html
<!-- Find sections like this: -->
<h2>About Infinite Streaming</h2>
<p class="about-text">Your new text here...</p>
```

### Update Styling

Modify the `<style>` section in `index.html`:

```css
/* Change colors */
.hero-section {
    background: linear-gradient(135deg, #YOUR-COLOR-1 0%, #YOUR-COLOR-2 100%);
}

/* Change fonts */
body {
    font-family: 'Your Font', sans-serif;
}
```

### Add/Remove Screenshots

1. Add image files to `screenshots/` directory
2. Update HTML:

```html
<div class="screenshot-container">
    <img src="../screenshots/your-new-image.png" alt="Description">
    <p class="screenshot-caption">Your caption</p>
</div>
```

### Optimize Images

Before deploying, consider optimizing images:

```bash
# Install ImageMagick
sudo apt-get install imagemagick

# Optimize PNG files (reduce file size)
for img in screenshots/*.png; do
    convert "$img" -quality 85 -strip "optimized-$img"
done
```

Or use online tools:
- https://tinypng.com/
- https://squoosh.app/

## Directory Structure

```
static-homepage/
├── index.html          # Main homepage file (HTML + CSS inline)
├── README.md           # This file
└── k8s-static-site.yaml # Kubernetes deployment (optional)
```

Note: Screenshots are referenced from parent directory `../screenshots/`

## Accessing the Site

After deployment:

- **k8s (nginx)**: http://lenovo.local:30082
- **Local dev**: http://localhost:8000
- **Custom domain**: Configure in your web server or hosting platform

## SSL/HTTPS Setup

### With k3s + cert-manager

1. Install cert-manager
2. Create Ingress with TLS (see k8s-static-site.yaml)
3. Let's Encrypt auto-provisions certificate

### With Cloudflare (Easiest)

1. Add domain to Cloudflare
2. Point A record to your server IP
3. Enable "Flexible SSL" in Cloudflare
4. Site will be https automatically

### With nginx + Let's Encrypt

```bash
# Install certbot
sudo apt-get install certbot python3-certbot-nginx

# Get certificate
sudo certbot --nginx -d infinitestreamer.com

# Auto-renewal is set up automatically
```

## Performance Tips

1. **Enable gzip compression** in web server config
2. **Use a CDN** (Cloudflare free tier)
3. **Optimize images** (see above)
4. **Minify HTML/CSS** (optional, but helps)

## Comparison: Static vs WordPress

| Feature | Static HTML | WordPress |
|---------|-------------|-----------|
| Speed | ⚡⚡⚡ Fastest | ⚡⚡ Fast with caching |
| Security | ✅ Very secure | ⚠️ Needs maintenance |
| Ease of setup | ✅ Very easy | ⚠️ Moderate |
| Ease of updates | ❌ Manual editing | ✅ GUI editor |
| Hosting cost | 💲 Free/cheap | 💲 Free to moderate |
| Scalability | ✅ Excellent | ⚡ Good with caching |
| SEO | ✅ Good | ✅ Excellent with plugins |

## Recommendation

- **Use WordPress** if you need frequent content updates or multiple editors
- **Use Static** if the site rarely changes and you prefer simplicity/speed

You can always start with static and migrate to WordPress later (or vice versa).
