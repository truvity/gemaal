<!-- Moved out of README.md unchanged; the README now links here. -->
# Development

Toolchain via [devbox](https://www.jetify.com/devbox/) (+ direnv), tasks
via [just](https://just.systems/):

```bash
just check      # build + test + lint + vuln — what CI runs
just generate   # regenerate gen/ from proto/ (buf; output is committed)
just run        # run the service skeleton against config.example.yaml
```

## License

[MIT](../../LICENSE)
