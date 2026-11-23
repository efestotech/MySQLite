# PHP client for the PDO scenario (docs/07-testing.md section 2): php:8.3-cli with
# pdo_mysql, which the base image does not enable.
FROM php:8.3-cli
RUN docker-php-ext-install pdo_mysql
