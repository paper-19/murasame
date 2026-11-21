# murasame docs

The source of the documentation site, built by GitHub Pages with Jekyll and the
[Just the Docs](https://just-the-docs.com) theme. This file is not part of the
site.

## Publish

On GitHub: Settings → Pages → Deploy from a branch → `main`, folder `/docs`.
Every push to `main` that touches `docs/` rebuilds the site.

## Preview locally

```sh
cd docs
docker run --rm -it -v "$PWD":/srv/jekyll -v murasame-docs-bundle:/usr/local/bundle -p 4000:4000 \
  jekyll/jekyll:4 sh -c 'bundle install && bundle exec jekyll serve --host 0.0.0.0 --force_polling'
```

Then open http://localhost:4000. With Ruby 3 installed instead:
`bundle install && bundle exec jekyll serve`.
